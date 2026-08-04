package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/simonvetter/modbus"
)

const (
	// modbusTimeout is the per-request Modbus timeout. Connection details,
	// poll interval, database path, and the device registry now come from
	// config.json (see config.go).
	modbusTimeout = 2 * time.Second

	// disabledUnitID marks an in-memory device with no exposed Modbus port
	// (config modbus_unit: null). Such devices are never polled.
	disabledUnitID = -1

	// Victron protocol facts, fixed by the device type rather than by the
	// installation. The aggregate shunt is a battery service, so it uses the
	// same register map as the individual banks (258=Power, 259=Voltage,
	// 261=Current) plus SOC at 266 — not the System map at 840.
	allBanksStartAddress  = 258
	allBanksRegisterCount = 4
	allBanksSOCAddress    = 266

	bankStartAddress  = 258
	bankRegisterCount = 4

	// The System service (com.victronenergy.system) exposes the pool aggregate
	// in a contiguous 840 block, with its own scaling: voltage /10 (not /100
	// like the battery service), current /10 (signed), power in whole watts, and
	// SOC as a whole percent (not /10). Calibrated against a known aggregate.
	systemStartAddress  = 840
	systemRegisterCount = 4
)

type AllBanksReading struct {
	Voltage float64
	Current float64
	Power   int16
	SOC     uint16
}

type BatteryBank struct {
	ID     int
	Name   string
	UnitID int

	// System marks the pool aggregate as sourced from the Venus System service
	// (unit 100 register map) rather than a battery shunt. Only meaningful on
	// the aggregate.
	System bool

	Voltage float64
	Current float64
	Power   int16
}

type SolarCharger struct {
	ID     int
	Name   string
	UnitID int

	BatteryVoltage float64
	BatteryCurrent float64

	PVVoltage float64
	PVCurrent float64
	PVPower   float64

	YieldToday    float64
	MaxPowerToday uint16

	ChargeState uint16
	MPPMode     uint16
	ErrorCode   uint16
}

// EnergyMeter is an AC energy meter polled from a MadBus instance over HTTP
// rather than from the Victron Modbus link. It carries only identity + routing
// (which instance, which MadBus device id); the readings themselves come fresh
// from MadBus each cycle.
type EnergyMeter struct {
	ID       int
	Name     string
	Source   string // MadBus source name (Config.Sources[].Name, type madbus)
	MadbusID string // device id within that instance
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// `sola healthcheck` probes the running dashboard and exits 0/1. It exists
	// so the distroless container (no shell, no curl) can define a HEALTHCHECK.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	// On a fresh install (e.g. an empty mounted volume) there is no config yet;
	// seed a valid default so the service can boot and be configured from there
	// rather than failing outright.
	path := configPath()
	if created, err := ensureDefaultConfig(path); err != nil {
		logger.Error("failed to write default configuration", "path", path, "error", err)
		os.Exit(1)
	} else if created {
		logger.Info(
			"wrote empty default configuration; open the dashboard and add a data source, then a device, under Settings → Data Sources",
			"path", path,
		)
	}

	// One-time on-disk upgrade of a legacy (pre-sources) config to the sources
	// schema, so the file a human reads matches what the code expects. LoadConfig
	// also migrates in memory every read, so this is purely cosmetic persistence.
	if err := migrateConfigFileOnce(path, logger); err != nil {
		logger.Error("failed to migrate configuration", "path", path, "error", err)
		os.Exit(1)
	}

	// An invalid (unparseable) config is still fatal: there is no prior good
	// state to fall back to, and guessing would be worse than a clear error.
	cfg, err := LoadConfig(path)
	if err != nil {
		logger.Error("failed to load configuration", "path", path, "error", err)
		os.Exit(1)
	}

	// MODBUS_URL overrides the modbus source's address so an operator can point
	// the container at their device without editing config. Kept for backward
	// compatibility; the source URL is now normally edited in the Settings UI.
	if url := os.Getenv("MODBUS_URL"); url != "" {
		applied := false
		for i := range cfg.Sources {
			if cfg.Sources[i].Type == SourceTypeModbus {
				if cfg.Sources[i].URL != url {
					logger.Info("modbus source URL overridden by MODBUS_URL", "source", cfg.Sources[i].Name, "url", url)
				}
				cfg.Sources[i].URL = url
				applied = true
				break
			}
		}
		if !applied {
			logger.Warn("MODBUS_URL set but no modbus source is configured; ignoring", "url", url)
		}
	}

	logger.Info("configuration loaded", "path", path, "devices", len(cfg.Devices))

	// The database is essential and is established once at startup.
	db, err := OpenDatabase(cfg.DatabasePath)
	if err != nil {
		logger.Error("failed to open database", "path", cfg.DatabasePath, "error", err)
		os.Exit(1)
	}

	if err := createSchema(db); err != nil {
		logger.Error("failed to create database schema", "error", err)
		os.Exit(1)
	}

	logger.Info("database ready", "path", cfg.DatabasePath)

	if err := seedDevices(db, cfg); err != nil {
		logger.Error("failed to seed device registry", "error", err)
		os.Exit(1)
	}

	// The dashboard comes up before — and independently of — the Modbus link,
	// so the UI is reachable even when the device is not. It reads the
	// current-status tables the poll loop maintains; changing http_addr
	// requires a restart.
	dashboard := StartDashboard(logger, db, cfg, path)

	// The device registry, poll interval, and debug flag are applied live (see
	// the reload below); modbus_url, database_path, and http_addr are fixed for
	// the process lifetime.
	aggregate, banks, charger, meters := buildDevices(cfg)

	// Resolve the (single, for now) Modbus source. A MadBus-only install has
	// none, in which case there is simply no Modbus client and only MadBus is
	// polled. The URL is fixed for the process lifetime (edits apply on restart).
	modbusURL := ""
	if src, ok := modbusSource(cfg); ok {
		modbusURL = src.URL
	}

	// The Modbus client is created once. A malformed URL is non-fatal: log it
	// and serve the dashboard anyway so the operator can correct the config,
	// rather than crash-looping.
	var client *modbus.ModbusClient
	if modbusURL != "" {
		if c, err := modbus.NewClient(&modbus.ClientConfiguration{
			URL:     modbusURL,
			Timeout: modbusTimeout,
		}); err != nil {
			logger.Error(
				"invalid modbus source URL; polling disabled until restart with a valid URL",
				"url", modbusURL,
				"error", err,
			)
		} else {
			client = c
		}
	}

	// Connect lazily and keep retrying: an unreachable device at startup, a
	// device reboot, or a network blip must never take the service down. Log
	// only on connection-state transitions to stay quiet during an outage.
	connected := false
	if client != nil {
		if err := client.Open(); err != nil {
			logger.Warn(
				"Victron Modbus server unreachable; dashboard is up, will keep retrying",
				"url", modbusURL,
				"error", err,
			)
		} else {
			connected = true
			logger.Info("connected to Victron Modbus server", "url", modbusURL)
		}
	}

	// History snapshots are captured from the poll loop (no separate goroutine)
	// at most once per history interval. The first successful poll records one.
	lastHistoryAt := time.Now()
	if connected {
		if !pollAndStore(logger, db, client, aggregate, banks, charger, cfg.Debug, true) {
			_ = client.Close()
			connected = false
		}
	}
	// MadBus is an independent source: poll it at startup even if Modbus failed.
	if len(meters) > 0 {
		pollMadbus(logger, db, cfg, meters, true)
	}

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	current := cfg
	configHealthy := true

	for {
		select {
		case <-ticker.C:
			// Re-read config each cycle. On failure keep the last-good copy,
			// logging only on the healthy->broken transition to avoid spamming
			// while the file is mid-edit.
			if fresh, err := LoadConfig(path); err != nil {
				if configHealthy {
					logger.Warn(
						"failed to reload configuration; keeping last-good",
						"path", path,
						"error", err,
					)
					configHealthy = false
				}
			} else {
				if !configHealthy {
					logger.Info("configuration reload recovered", "path", path)
					configHealthy = true
				}

				if fresh.PollIntervalSeconds != current.PollIntervalSeconds {
					ticker.Reset(time.Duration(fresh.PollIntervalSeconds) * time.Second)
					logger.Info(
						"poll interval changed",
						"seconds", fresh.PollIntervalSeconds,
					)
				}

				// Apply device add/edit/delete live. Rebuilding the registry
				// here — in the poll goroutine that owns the Modbus client and
				// device structs — keeps all device state single-threaded; the
				// web API only ever rewrites config.json.
				if !reflect.DeepEqual(fresh.Devices, current.Devices) {
					aggregate, banks, charger, meters = reconcileDevices(logger, db, current, fresh)
				}

				// Apply a change to the Modbus source live: first appearance (the
				// user adds their Victron source on a fresh install), a URL edit, or
				// removal. The client is otherwise built once at startup, so without
				// this a newly-added source wouldn't poll until a restart. MODBUS_URL
				// still wins if set. On (re)build the reconnect block below opens it
				// this same tick.
				applyModbusURLEnv(&fresh)
				freshURL := ""
				if src, ok := modbusSource(fresh); ok {
					freshURL = src.URL
				}
				if freshURL != modbusURL {
					if client != nil {
						_ = client.Close()
						client = nil
						connected = false
					}
					modbusURL = freshURL
					if modbusURL == "" {
						logger.Info("Modbus source removed; polling stopped")
					} else if c, err := modbus.NewClient(&modbus.ClientConfiguration{
						URL:     modbusURL,
						Timeout: modbusTimeout,
					}); err != nil {
						logger.Error("invalid Modbus source URL; polling disabled until corrected", "url", modbusURL, "error", err)
					} else {
						client = c
						logger.Info("Modbus source changed; connecting on next cycle", "url", modbusURL)
					}
				}

				current = fresh
			}

			// Record a history snapshot once per (hot-appliable) history interval.
			// It's a floor: snapshots ride poll cycles, so they can't be closer
			// together than the poll interval. Computed once per tick and shared by
			// both data sources, so meter history is captured even when Modbus is
			// down (and vice-versa).
			recordHistory := time.Since(lastHistoryAt) >= time.Duration(current.HistoryIntervalSec)*time.Second
			if recordHistory {
				lastHistoryAt = time.Now()
			}

			// Keep the Modbus link healthy: (re)connect if needed, then poll. A
			// poll that fails wholesale means the link dropped, so close and
			// reconnect on the next tick. Logging is transition-only so a
			// sustained outage does not spam.
			if client != nil {
				if !connected {
					if err := client.Open(); err == nil {
						connected = true
						logger.Info("reconnected to Victron Modbus server", "url", modbusURL)
					}
				}

				if connected {
					if !pollAndStore(logger, db, client, aggregate, banks, charger, current.Debug, recordHistory) {
						logger.Warn("Modbus link lost; will reconnect", "url", modbusURL)
						_ = client.Close()
						connected = false
					}
				}
			}

			// MadBus is a separate data source: poll it every tick regardless of
			// the Modbus link state. Its failures mark only its own meters offline
			// and never disturb the Modbus connection.
			if len(meters) > 0 {
				pollMadbus(logger, db, current, meters, recordHistory)
			}

		case <-ctx.Done():
			logger.Info("shutdown signal received")

			shutdownDashboard(dashboard, logger)

			if client != nil {
				if err := client.Close(); err != nil {
					logger.Error(
						"failed to close Modbus connection",
						"error", err,
					)
				} else {
					logger.Info("Modbus connection closed")
				}
			}

			if err := db.Close(); err != nil {
				logger.Error("failed to close database", "error", err)
			} else {
				logger.Info("database closed")
			}

			logger.Info("Sola stopped")
			return
		}
	}
}

// runHealthcheck probes the local dashboard's /api/status and returns a process
// exit code (0 healthy, 1 not). It backs the container HEALTHCHECK, since the
// distroless image has no shell or curl to run one the usual way.
func runHealthcheck() int {
	addr := defaultHTTPAddr
	if cfg, err := LoadConfig(configPath()); err == nil && cfg.HTTPAddr != "" {
		addr = cfg.HTTPAddr
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8088"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/api/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unexpected status", resp.StatusCode)
		return 1
	}

	return 0
}

// buildDevices turns the configured device list into the in-memory structures
// the poll loop uses: the single aggregate shunt (nil if none), the individual
// banks, and the charge controller (nil if none). validate() has already
// guaranteed device types are valid and at most one aggregate exists.
func buildDevices(cfg Config) (aggregate *BatteryBank, banks []BatteryBank, charger *SolarCharger, meters []EnergyMeter) {
	for _, d := range cfg.Devices {
		switch d.DeviceType {
		case DeviceTypeEnergyMeter:
			meters = append(meters, EnergyMeter{
				ID:       d.ID,
				Name:     d.Name,
				Source:   d.Source,
				MadbusID: d.MadbusID,
			})

		case DeviceTypeShunt:
			bank := BatteryBank{
				ID:     d.ID,
				Name:   d.Name,
				UnitID: unitOrDisabled(d.ModbusUnit),
			}
			if d.Aggregate {
				agg := bank
				aggregate = &agg
			} else {
				banks = append(banks, bank)
			}

		case DeviceTypeChargeController:
			charger = &SolarCharger{
				ID:     d.ID,
				Name:   d.Name,
				UnitID: unitOrDisabled(d.ModbusUnit),
			}

		case DeviceTypeSystem:
			// The System service is the pool aggregate, read from its own
			// register map (System: true).
			aggregate = &BatteryBank{
				ID:     d.ID,
				Name:   d.Name,
				UnitID: unitOrDisabled(d.ModbusUnit),
				System: true,
			}
		}
	}

	return aggregate, banks, charger, meters
}

// reconcileDevices applies a changed device list live: it deletes the status
// rows of devices that were removed, refreshes/seeds identities for the current
// set, and returns the rebuilt in-memory registry. It runs in the poll loop so
// no other goroutine touches device state. Row/seed failures are logged but not
// fatal — the collector keeps running with whatever succeeded.
func reconcileDevices(logger *slog.Logger, db *sql.DB, old, fresh Config) (*BatteryBank, []BatteryBank, *SolarCharger, []EnergyMeter) {
	freshIDs := make(map[int]bool, len(fresh.Devices))
	for _, d := range fresh.Devices {
		freshIDs[d.ID] = true
	}

	for _, d := range old.Devices {
		if freshIDs[d.ID] {
			continue
		}

		if err := deleteDevice(db, statusTable(d.DeviceType), d.ID); err != nil {
			logger.Error("failed to remove device row", "id", d.ID, "name", d.Name, "error", err)
		} else {
			logger.Info("device removed", "id", d.ID, "name", d.Name)
		}
	}

	if err := seedDevices(db, fresh); err != nil {
		logger.Error("failed to seed device registry after reload", "error", err)
	}

	logger.Info("device registry reloaded", "devices", len(fresh.Devices))

	return buildDevices(fresh)
}

// unitOrDisabled converts a configured (nullable) Modbus unit into the
// in-memory representation, treating a null port as the disabled sentinel.
func unitOrDisabled(unit *int) int {
	if unit == nil {
		return disabledUnitID
	}

	return *unit
}

// modbusSource returns the single configured Modbus source, if any. This
// version supports at most one (enforced by validate); Phase 2 will handle
// several with per-source clients.
func modbusSource(cfg Config) (Source, bool) {
	for _, s := range cfg.Sources {
		if s.Type == SourceTypeModbus {
			return s, true
		}
	}
	return Source{}, false
}

// applyModbusURLEnv overrides the modbus source's URL with the MODBUS_URL env
// var when set (legacy back-compat). It mutates cfg in place and is silent — the
// startup path logs the override once, and the reload path applies it every
// tick so a reloaded config keeps honoring the env.
func applyModbusURLEnv(cfg *Config) {
	url := os.Getenv("MODBUS_URL")
	if url == "" {
		return
	}
	for i := range cfg.Sources {
		if cfg.Sources[i].Type == SourceTypeModbus {
			cfg.Sources[i].URL = url
			return
		}
	}
}

// testModbusConnection verifies a Modbus-TCP endpoint is reachable by opening
// (and closing) a connection to it. It deliberately does not read registers:
// unit/register mapping belongs to individual devices, so at the source level a
// success means "a Modbus-TCP server is reachable at this address" — which is
// what the "Test Connection" button promises before a source is saved.
func testModbusConnection(url string) error {
	client, err := modbus.NewClient(&modbus.ClientConfiguration{URL: url, Timeout: modbusTimeout})
	if err != nil {
		return err
	}
	if err := client.Open(); err != nil {
		return err
	}
	return client.Close()
}

// testMadbusConnection verifies a MadBus endpoint answers by performing a real
// measurements round-trip (empty selector = all devices). Reuses the same path
// the poll loop and device discovery use, so a success here means polling works.
func testMadbusConnection(url string) error {
	_, err := madbusMeasurements(url, nil)
	return err
}

// testModbusUnit verifies a specific Modbus unit (slave) ID responds on a source
// by actually reading the registers that device type polls. Modbus has no unit
// enumeration, so this read is the only real proof a unit exists — a plain TCP
// connect (testModbusConnection) only proves the gateway is up, not that the
// unit answers. Used by the device form's per-unit "Test Connection" gate.
func testModbusUnit(url, deviceType string, unit int) error {
	client, err := modbus.NewClient(&modbus.ClientConfiguration{URL: url, Timeout: modbusTimeout})
	if err != nil {
		return err
	}
	if err := client.Open(); err != nil {
		return err
	}
	defer client.Close()

	switch deviceType {
	case DeviceTypeSystem:
		_, err = readSystem(client, unit)
	case DeviceTypeShunt:
		// Reading the battery-service block proves the unit answers; that's the
		// core of both the per-bank and aggregate reads.
		err = readBatteryBank(client, &BatteryBank{UnitID: unit})
	case DeviceTypeChargeController:
		err = readSolarCharger(client, &SolarCharger{UnitID: unit})
	default:
		return fmt.Errorf("device type %q is not read over Modbus", deviceType)
	}
	return err
}

// statusTable maps a device type to its current-status table. Shunt/system
// share the battery table; charge controllers and energy meters have their own.
func statusTable(deviceType string) string {
	switch deviceType {
	case DeviceTypeChargeController:
		return tableChargeController
	case DeviceTypeEnergyMeter:
		return tableEnergyMeter
	default:
		return tableBatteryShunt
	}
}

// seedDevices registers every configured device in its status table so that
// even never-polled devices (such as disconnected banks) are visible as
// offline from startup. Existing rows are left untouched.
func seedDevices(db *sql.DB, cfg Config) error {
	for _, d := range cfg.Devices {
		// MadBus-sourced devices have a string (source, madbus_id) identity
		// instead of a Modbus unit, so they seed via a dedicated path.
		if d.DeviceType == DeviceTypeEnergyMeter {
			if err := seedEnergyMeter(db, d.ID, d.Source, d.MadbusID, d.Name); err != nil {
				return err
			}
			continue
		}

		if err := seedDevice(
			db, statusTable(d.DeviceType), d.ID, modbusID(unitOrDisabled(d.ModbusUnit)), d.Name,
		); err != nil {
			return err
		}
	}

	return nil
}

// modbusID converts an in-memory unit ID into a nullable database value,
// treating the disabled sentinel as "no Modbus mapping" (NULL).
func modbusID(unitID int) sql.NullInt64 {
	if unitID == disabledUnitID {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: int64(unitID), Valid: true}
}

// --- MadBus polling ----------------------------------------------------------

// madbusTimeout bounds a single MadBus HTTP round-trip, independent of the
// Modbus timeout: a slow or down MadBus instance must not stall the poll loop.
const madbusTimeout = 4 * time.Second

// madbusClient is the shared HTTP client for MadBus polls (connection reuse).
var madbusClient = &http.Client{Timeout: madbusTimeout}

// MadBus measurements API DTOs (mirror the MadBus service's internal/api).
type madbusMeasurement struct {
	Value *float64 `json:"value"`
	Unit  string   `json:"unit"`
	Stale bool     `json:"stale"`
}

type madbusDevice struct {
	Device struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Profile  string  `json:"profile"`
		Online   bool    `json:"online"`
		LastRead *string `json:"last_read"`
	} `json:"device"`
	Measurements map[string]madbusMeasurement `json:"measurements"`
}

type madbusResponse struct {
	ReadAt  string         `json:"read_at"`
	Devices []madbusDevice `json:"devices"`
}

type madbusSelector struct {
	ID string `json:"id"`
}

type madbusRequest struct {
	Devices []madbusSelector `json:"devices"`
}

// pollMadbus reads every configured energy meter from its MadBus instance and
// stores the results. Meters are grouped by instance so each instance is polled
// exactly once per cycle (one round-trip for all of its devices). An instance
// that can't be reached marks only its own meters offline — it never touches the
// Victron Modbus link. Values are rounded on the way in (MadBus serves float32
// promoted to float64, so raw values carry fuzz like 0.20800000429153442).
func pollMadbus(logger *slog.Logger, db *sql.DB, cfg Config, meters []EnergyMeter, recordHistory bool) {
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	urls := make(map[string]string)
	for _, s := range cfg.Sources {
		if s.Type == SourceTypeMadbus {
			urls[s.Name] = s.URL
		}
	}

	byInstance := make(map[string][]EnergyMeter)
	for _, m := range meters {
		byInstance[m.Source] = append(byInstance[m.Source], m)
	}

	for source, group := range byInstance {
		url, ok := urls[source]
		if !ok {
			// Instance vanished from config (mid-edit): keep last-good, show offline.
			markMetersOffline(logger, db, group)
			continue
		}

		results, err := readMadbus(url, group)
		if err != nil {
			logger.Warn("MadBus poll failed", "source", source, "url", url, "error", err)
			markMetersOffline(logger, db, group)
			continue
		}

		for _, m := range group {
			dev, present := results[m.MadbusID]
			if !present || !dev.Device.Online {
				// Absent or reported offline: preserve the last-good reading and
				// just flip status to offline.
				if err := markDeviceOffline(db, tableEnergyMeter, m.ID); err != nil {
					logger.Error("failed to mark meter offline", "id", m.ID, "error", err)
				}
				continue
			}

			status := buildMeterStatus(m, dev)
			if cfg.Debug {
				printMeter(status)
			}
			if err := upsertEnergyMeter(db, status, updatedAt); err != nil {
				logger.Error("failed to store meter reading", "id", m.ID, "error", err)
			}
			if recordHistory {
				if err := insertEnergyMeterHistory(db, status, updatedAt); err != nil {
					logger.Error("failed to record meter history", "id", m.ID, "error", err)
				}
			}
		}
	}
}

// markMetersOffline flags a whole group of meters offline (used when their
// MadBus instance is unreachable), preserving each one's last-good reading.
func markMetersOffline(logger *slog.Logger, db *sql.DB, meters []EnergyMeter) {
	for _, m := range meters {
		if err := markDeviceOffline(db, tableEnergyMeter, m.ID); err != nil {
			logger.Error("failed to mark meter offline", "id", m.ID, "error", err)
		}
	}
}

// readMadbus performs one MadBus measurements round-trip for the given meters
// and returns the response devices keyed by their MadBus id.
func readMadbus(baseURL string, meters []EnergyMeter) (map[string]madbusDevice, error) {
	sels := make([]madbusSelector, 0, len(meters))
	for _, m := range meters {
		sels = append(sels, madbusSelector{ID: m.MadbusID})
	}

	devs, err := madbusMeasurements(baseURL, sels)
	if err != nil {
		return nil, err
	}

	devices := make(map[string]madbusDevice, len(devs))
	for _, d := range devs {
		devices[d.Device.ID] = d
	}

	return devices, nil
}

// madbusBaseURL normalizes a MadBus source URL so it always carries a scheme.
// Users commonly enter "host:port" (e.g. 192.168.1.3:8090) without "http://";
// url.Parse then reads the host as a path segment and rejects the colon. MadBus
// is plain HTTP, so a missing scheme defaults to http://.
func madbusBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}

// madbusMeasurements performs one MadBus measurements round-trip for the given
// selectors and returns the response devices. An empty selector list asks MadBus
// for every device it serves, which is how the device form discovers the ids a
// source offers (see handleListSourceDevices).
func madbusMeasurements(baseURL string, sels []madbusSelector) ([]madbusDevice, error) {
	body, err := json.Marshal(madbusRequest{Devices: sels})
	if err != nil {
		return nil, fmt.Errorf("encode madbus request: %w", err)
	}

	endpoint := strings.TrimRight(madbusBaseURL(baseURL), "/") + "/api/v1/measurements"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build madbus request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := madbusClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("madbus returned HTTP %d", resp.StatusCode)
	}

	var out madbusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode madbus response: %w", err)
	}

	return out.Devices, nil
}

// buildMeterStatus maps a MadBus device's measurements into an EnergyMeterStatus,
// rounding each metric to a sensible precision. status is "stale" if MadBus
// flagged any measurement stale (values kept, but shown as not-fresh), else
// "online". The caller has already established the device is online.
func buildMeterStatus(m EnergyMeter, dev madbusDevice) EnergyMeterStatus {
	stale := false
	for _, mm := range dev.Measurements {
		if mm.Stale {
			stale = true
			break
		}
	}

	status := "online"
	if stale {
		status = "stale"
	}

	return EnergyMeterStatus{
		ID:            m.ID,
		Source:        m.Source,
		MadbusID:      m.MadbusID,
		Name:          m.Name,
		Voltage:       meterMetric(dev, "ac.voltage", 1),
		Current:       meterMetric(dev, "ac.current", 2),
		Frequency:     meterMetric(dev, "ac.frequency", 2),
		Power:         meterMetric(dev, "ac.power", 1),
		ApparentPower: meterMetric(dev, "ac.power.apparent", 1),
		ReactivePower: meterMetric(dev, "ac.power.reactive", 1),
		PowerFactor:   meterMetric(dev, "ac.power_factor", 3),

		// Per-leg (split-phase). Absent keys stay NULL, so a single-phase meter
		// simply reports none of these.
		CurrentL1:       meterMetric(dev, "ac.current.l1", 2),
		CurrentL2:       meterMetric(dev, "ac.current.l2", 2),
		PowerL1:         meterMetric(dev, "ac.power.l1", 1),
		PowerL2:         meterMetric(dev, "ac.power.l2", 1),
		ApparentPowerL1: meterMetric(dev, "ac.power.apparent.l1", 1),
		ApparentPowerL2: meterMetric(dev, "ac.power.apparent.l2", 1),
		ReactivePowerL1: meterMetric(dev, "ac.power.reactive.l1", 1),
		ReactivePowerL2: meterMetric(dev, "ac.power.reactive.l2", 1),
		PowerFactorL1:   meterMetric(dev, "ac.power_factor.l1", 3),
		PowerFactorL2:   meterMetric(dev, "ac.power_factor.l2", 3),

		EnergyImport: meterMetric(dev, "ac.energy.import", 2),
		EnergyExport: meterMetric(dev, "ac.energy.export", 2),
		EnergyTotal:  meterMetric(dev, "ac.energy.total", 2),
		Status:       status,
	}
}

// meterMetric extracts and rounds one measurement, returning SQL NULL when the
// metric is absent or its value is null.
func meterMetric(dev madbusDevice, key string, decimals int) sql.NullFloat64 {
	mm, ok := dev.Measurements[key]
	if !ok || mm.Value == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: roundTo(*mm.Value, decimals), Valid: true}
}

// roundTo rounds v to the given number of decimal places.
func roundTo(v float64, decimals int) float64 {
	scale := math.Pow(10, float64(decimals))
	return math.Round(v*scale) / scale
}

// printMeter logs one meter reading when debug is enabled (mirrors the Victron
// printX debug helpers).
func printMeter(m EnergyMeterStatus) {
	nf := func(n sql.NullFloat64) string {
		if !n.Valid {
			return "—"
		}
		return fmt.Sprintf("%g", n.Float64)
	}
	fmt.Printf("[meter %s] status=%s power=%sW V=%s A=%s Hz=%s pf=%s total=%skWh\n",
		m.Name, m.Status, nf(m.Power), nf(m.Voltage), nf(m.Current),
		nf(m.Frequency), nf(m.PowerFactor), nf(m.EnergyTotal))
}

func pollAndStore(
	logger *slog.Logger,
	db *sql.DB,
	client *modbus.ModbusClient,
	aggregate *BatteryBank,
	banks []BatteryBank,
	solarCharger *SolarCharger,
	debug bool,
	recordHistory bool,
) bool {
	// One timestamp for the whole poll so every row updated in this cycle
	// shares the same reading time.
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	// Track read outcomes so the caller can distinguish a dropped link (every
	// read failed) from a single misconfigured device (some reads succeeded).
	attempted, failed := 0, 0

	if aggregate != nil {
		// The aggregate reads from the System register map when sourced from the
		// Venus System service, and from the battery-service map otherwise.
		readAggregate := readAllBanks
		if aggregate.System {
			readAggregate = readSystem
		}

		attempted++
		allBanks, err := readAggregate(client, aggregate.UnitID)
		if err != nil {
			failed++
			logger.Error(
				"failed to read All Banks registers",
				"unit_id", aggregate.UnitID,
				"error", err,
			)

			if err := markDeviceOffline(db, tableBatteryShunt, aggregate.ID); err != nil {
				logger.Error("failed to mark All Banks offline", "error", err)
			}
		} else {
			if debug {
				printAllBanks(aggregate.Name, allBanks)
			}

			shunt := ShuntStatus{
				ID:       aggregate.ID,
				ModbusID: aggregate.UnitID,
				Name:     aggregate.Name,
				Voltage:  allBanks.Voltage,
				Current:  allBanks.Current,
				Wattage:  int(allBanks.Power),
				// The aggregate owns the pool SOC.
				SOC: sql.NullInt64{Int64: int64(allBanks.SOC), Valid: true},
			}

			if err := upsertBatteryShunt(db, shunt, updatedAt); err != nil {
				logger.Error("failed to store All Banks reading", "error", err)
			}

			if recordHistory {
				if err := insertShuntHistory(db, shunt, updatedAt); err != nil {
					logger.Error("failed to record All Banks history", "error", err)
				}
			}
		}
	}

	for i := range banks {
		if banks[i].UnitID == disabledUnitID {
			continue
		}

		attempted++
		if err := readBatteryBank(client, &banks[i]); err != nil {
			failed++
			logger.Error(
				"failed to read battery bank",
				"bank", banks[i].Name,
				"unit_id", banks[i].UnitID,
				"error", err,
			)

			if err := markDeviceOffline(db, tableBatteryShunt, banks[i].ID); err != nil {
				logger.Error(
					"failed to mark battery bank offline",
					"bank", banks[i].Name,
					"error", err,
				)
			}
			continue
		}

		if debug {
			printBatteryBank(banks[i])
		}

		shunt := ShuntStatus{
			ID:       banks[i].ID,
			ModbusID: banks[i].UnitID,
			Name:     banks[i].Name,
			Voltage:  banks[i].Voltage,
			Current:  banks[i].Current,
			Wattage:  int(banks[i].Power),
			// SOC left NULL: an individual bank is not the pool source of truth.
		}

		if err := upsertBatteryShunt(db, shunt, updatedAt); err != nil {
			logger.Error(
				"failed to store battery bank reading",
				"bank", banks[i].Name,
				"error", err,
			)
		}

		if recordHistory {
			if err := insertShuntHistory(db, shunt, updatedAt); err != nil {
				logger.Error(
					"failed to record battery bank history",
					"bank", banks[i].Name,
					"error", err,
				)
			}
		}
	}

	// A configuration may have no charge controller at all (it was never added,
	// or was deleted live). Skip the read entirely rather than dereferencing a
	// nil charger.
	if solarCharger != nil {
		attempted++
		if err := readSolarCharger(client, solarCharger); err != nil {
			failed++
			logger.Error(
				"failed to read solar charger",
				"charger", solarCharger.Name,
				"unit_id", solarCharger.UnitID,
				"error", err,
			)

			if err := markDeviceOffline(db, tableChargeController, solarCharger.ID); err != nil {
				logger.Error("failed to mark charge controller offline", "error", err)
			}
		} else {
			if debug {
				printSolarCharger(*solarCharger)
			}

			controller := ChargeControllerStatus{
				ID:             solarCharger.ID,
				ModbusID:       solarCharger.UnitID,
				Name:           solarCharger.Name,
				BatteryVoltage: solarCharger.BatteryVoltage,
				BatteryCurrent: solarCharger.BatteryCurrent,
				PVVoltage:      solarCharger.PVVoltage,
				PVCurrent:      solarCharger.PVCurrent,
				PVPower:        solarCharger.PVPower,
				YieldToday:     solarCharger.YieldToday,
				MaxPowerToday:  int(solarCharger.MaxPowerToday),
				ChargeState:    int(solarCharger.ChargeState),
				MPPMode:        int(solarCharger.MPPMode),
				ErrorCode:      int(solarCharger.ErrorCode),
			}

			if err := upsertChargeController(db, controller, updatedAt); err != nil {
				logger.Error("failed to store charge controller reading", "error", err)
			}

			if recordHistory {
				if err := insertChargeControllerHistory(db, controller, updatedAt); err != nil {
					logger.Error("failed to record charge controller history", "error", err)
				}
			}
		}
	}

	// Blank line separates one poll's readings from the next in debug output.
	if debug {
		fmt.Println()
	}

	// Healthy unless every attempted read failed (or there was nothing to
	// read): a partial failure is a per-device problem, not a dead link.
	return !(attempted > 0 && failed == attempted)
}

func readAllBanks(
	client *modbus.ModbusClient,
	unitID int,
) (AllBanksReading, error) {
	client.SetUnitId(uint8(unitID))

	registers, err := client.ReadRegisters(
		allBanksStartAddress,
		allBanksRegisterCount,
		modbus.HOLDING_REGISTER,
	)
	if err != nil {
		return AllBanksReading{}, err
	}

	if len(registers) != allBanksRegisterCount {
		return AllBanksReading{}, fmt.Errorf(
			"unexpected register count: expected %d, received %d",
			allBanksRegisterCount,
			len(registers),
		)
	}

	// SOC is reported outside the 258 block, in the battery service's own
	// register, and is scaled by 10.
	socRegisters, err := readRegisterBlock(client, allBanksSOCAddress, 1)
	if err != nil {
		return AllBanksReading{}, fmt.Errorf("read SOC register: %w", err)
	}

	return AllBanksReading{
		Power:   int16(registers[0]),          // 258
		Voltage: float64(registers[1]) * 0.01, // 259
		// registers[2] is Modbus address 260 and is not used.
		Current: float64(int16(registers[3])) * 0.1, // 261
		SOC:     socRegisters[0] / 10,               // 266
	}, nil
}

// readSystem reads the pool aggregate from the Venus System service. Its 840
// block is contiguous, so a single read covers voltage/current/power/SOC. The
// scaling differs from the battery service (see the systemStartAddress note).
func readSystem(
	client *modbus.ModbusClient,
	unitID int,
) (AllBanksReading, error) {
	client.SetUnitId(uint8(unitID))

	registers, err := readRegisterBlock(client, systemStartAddress, systemRegisterCount)
	if err != nil {
		return AllBanksReading{}, err
	}

	return AllBanksReading{
		Voltage: float64(registers[0]) * 0.1,        // 840
		Current: float64(int16(registers[1])) * 0.1, // 841
		Power:   int16(registers[2]),                // 842
		SOC:     registers[3],                       // 843 (whole percent)
	}, nil
}

func readBatteryBank(
	client *modbus.ModbusClient,
	bank *BatteryBank,
) error {
	client.SetUnitId(uint8(bank.UnitID))

	registers, err := client.ReadRegisters(
		bankStartAddress,
		bankRegisterCount,
		modbus.HOLDING_REGISTER,
	)
	if err != nil {
		return err
	}

	if len(registers) != bankRegisterCount {
		return fmt.Errorf(
			"unexpected register count: expected %d, received %d",
			bankRegisterCount,
			len(registers),
		)
	}

	bank.Power = int16(registers[0])
	bank.Voltage = float64(registers[1]) * 0.01

	// registers[2] is Modbus address 260 and is not used.
	bank.Current = float64(int16(registers[3])) * 0.1

	return nil
}

func readSolarCharger(
	client *modbus.ModbusClient,
	charger *SolarCharger,
) error {
	client.SetUnitId(uint8(charger.UnitID))

	// 771: Battery voltage
	// 772: Battery current
	batteryRegisters, err := readRegisterBlock(client, 771, 2)
	if err != nil {
		return fmt.Errorf("read battery output registers: %w", err)
	}

	charger.BatteryVoltage = float64(batteryRegisters[0]) / 100
	charger.BatteryCurrent = float64(int16(batteryRegisters[1])) / 10

	// 775: Charge state
	// 776: PV voltage
	// 777: PV current
	pvRegisters, err := readRegisterBlock(client, 775, 3)
	if err != nil {
		return fmt.Errorf("read PV registers: %w", err)
	}

	charger.ChargeState = pvRegisters[0]
	charger.PVVoltage = float64(pvRegisters[1]) / 100
	charger.PVCurrent = float64(int16(pvRegisters[2])) / 10

	// 784: Yield today
	// 785: Maximum charge power today
	historyRegisters, err := readRegisterBlock(client, 784, 2)
	if err != nil {
		return fmt.Errorf("read daily history registers: %w", err)
	}

	charger.YieldToday = float64(historyRegisters[0]) / 10
	charger.MaxPowerToday = historyRegisters[1]

	// 788: Error code
	// 789: PV power
	// 790: User yield, not currently used
	// 791: MPP operation mode
	statusRegisters, err := readRegisterBlock(client, 788, 4)
	if err != nil {
		return fmt.Errorf("read charger status registers: %w", err)
	}

	charger.ErrorCode = statusRegisters[0]
	charger.PVPower = float64(statusRegisters[1]) / 10
	charger.MPPMode = statusRegisters[3]

	return nil
}

func readRegisterBlock(
	client *modbus.ModbusClient,
	startAddress uint16,
	registerCount uint16,
) ([]uint16, error) {
	registers, err := client.ReadRegisters(
		startAddress,
		registerCount,
		modbus.HOLDING_REGISTER,
	)
	if err != nil {
		return nil, err
	}

	if len(registers) != int(registerCount) {
		return nil, fmt.Errorf(
			"unexpected register count: expected %d, received %d",
			registerCount,
			len(registers),
		)
	}

	return registers, nil
}

func printAllBanks(name string, reading AllBanksReading) {
	fmt.Printf(
		"%s | %-10s | Voltage: %.1f V | Current: %.1f A | Power: %d W | SOC: %d%%\n",
		currentTime(),
		name,
		reading.Voltage,
		reading.Current,
		reading.Power,
		reading.SOC,
	)
}

func printBatteryBank(bank BatteryBank) {
	fmt.Printf(
		"%s | %-10s | Voltage: %.2f V | Current: %.1f A | Power: %d W\n",
		currentTime(),
		bank.Name,
		bank.Voltage,
		bank.Current,
		bank.Power,
	)
}

func printSolarCharger(charger SolarCharger) {
	fmt.Printf(
		"%s | %-10s | PV: %.2f V, %.1f A, %.1f W | Battery: %.2f V, %.1f A | State: %s | Yield: %.1f kWh | Peak: %d W | MPPT: %s | Error: %s\n",
		currentTime(),
		charger.Name,
		charger.PVVoltage,
		charger.PVCurrent,
		charger.PVPower,
		charger.BatteryVoltage,
		charger.BatteryCurrent,
		chargeStateName(charger.ChargeState),
		charger.YieldToday,
		charger.MaxPowerToday,
		mppModeName(charger.MPPMode),
		chargerErrorName(charger.ErrorCode),
	)
}

func currentTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func chargeStateName(state uint16) string {
	switch state {
	case 0:
		return "Off"
	case 2:
		return "Fault"
	case 3:
		return "Bulk"
	case 4:
		return "Absorption"
	case 5:
		return "Float"
	case 6:
		return "Storage"
	case 7:
		return "Equalize"
	case 11:
		return "Other"
	case 252:
		return "External control"
	default:
		return fmt.Sprintf("Unknown (%d)", state)
	}
}

func mppModeName(mode uint16) string {
	switch mode {
	case 0:
		return "Off"
	case 1:
		return "Limited"
	case 2:
		return "Active"
	case 255:
		return "Unavailable"
	default:
		return fmt.Sprintf("Unknown (%d)", mode)
	}
}

func chargerErrorName(code uint16) string {
	if code == 0 {
		return "None"
	}

	return fmt.Sprintf("Code %d", code)
}
