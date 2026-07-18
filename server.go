package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// webFiles holds the dashboard's static assets. Embedding them keeps the
// collector a single deployable binary: there are no loose files to lose or
// keep in sync on the target machine.
//
//go:embed web/solar_dashboard.html web/style.css web/dashboard.js web/background.js web/devices.html web/devices.js web/device.html web/device.js web/settings.html web/settings.js
var webFiles embed.FS

// dashboardServer serves the read-only web dashboard: the static page and a
// single JSON endpoint that reflects the current-status tables. It only reads
// the database; all writes remain the poll loop's job.
type dashboardServer struct {
	db         *sql.DB
	logger     *slog.Logger
	configPath string

	// writeMu serialises config read-modify-write so two concurrent device
	// edits cannot clobber each other.
	writeMu sync.Mutex

	// lastConfig caches the most recent successfully-loaded config. Config-
	// derived facts (which shunt is the aggregate, charger max amps, the SOC
	// threshold) are re-read from disk per request so live edits are reflected;
	// this cache is the fallback if a read momentarily fails.
	mu         sync.Mutex
	lastConfig Config
}

// StartDashboard builds the HTTP server, begins listening in the background,
// and returns it so the caller can shut it down. A failure to bind is fatal to
// the dashboard but not to the collector, so it is logged rather than returned.
func StartDashboard(logger *slog.Logger, db *sql.DB, cfg Config, configPath string) *http.Server {
	handler := &dashboardServer{
		db:         db,
		logger:     logger,
		configPath: configPath,
		lastConfig: cfg,
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler.routes(),
	}

	go func() {
		logger.Info("dashboard listening", "addr", cfg.HTTPAddr)

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("dashboard server stopped", "error", err)
		}
	}()

	return srv
}

// aggregateIDs returns the set of device IDs that provide the pool aggregate —
// either an aggregate-flagged battery shunt or a System device (implicitly the
// aggregate). The dashboard renders these in the Battery Pool pane.
func aggregateIDs(cfg Config) map[int]bool {
	ids := make(map[int]bool)
	for _, d := range cfg.Devices {
		if d.DeviceType == DeviceTypeSystem || (d.DeviceType == DeviceTypeShunt && d.Aggregate) {
			ids[d.ID] = true
		}
	}

	return ids
}

// chargerMaxAmperage returns each charger's configured rated output amps, keyed
// by device ID. Chargers with no configured value are omitted.
func chargerMaxAmperage(cfg Config) map[int]float64 {
	amps := make(map[int]float64)
	for _, d := range cfg.Devices {
		if d.DeviceType == DeviceTypeChargeController && d.MaxAmperage != nil {
			amps[d.ID] = *d.MaxAmperage
		}
	}

	return amps
}

func (s *dashboardServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", s.handleStatus)

	// Device registry CRUD. These only ever rewrite config.json; the poll loop
	// applies the change (and deletes removed rows) on its next cycle.
	mux.HandleFunc("GET /api/devices", s.handleListDevices)
	mux.HandleFunc("POST /api/devices", s.handleCreateDevice)
	mux.HandleFunc("PUT /api/devices/{id}", s.handleUpdateDevice)
	mux.HandleFunc("DELETE /api/devices/{id}", s.handleDeleteDevice)

	// Display settings (currently just the background).
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)

	// Serve the embedded assets by name, mapping the clean page paths to their
	// backing HTML files.
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		// The sub-filesystem is built from a compile-time embed path, so this
		// can only fail if the embed directive and this string disagree.
		panic(fmt.Sprintf("web assets not embedded: %v", err))
	}

	fileServer := http.FileServer(http.FS(static))

	pages := map[string]string{
		"/":         "/solar_dashboard.html",
		"/devices":  "/devices.html",
		"/device":   "/device.html",
		"/settings": "/settings.html",
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if file, ok := pages[r.URL.Path]; ok {
			r.URL.Path = file
		}

		fileServer.ServeHTTP(w, r)
	})

	return mux
}

// shuntJSON is one battery shunt as sent to the client. Pointer fields carry
// SQL NULL through as JSON null, so a never-read device is visibly missing its
// readings rather than reporting a misleading zero.
type shuntJSON struct {
	ID        int      `json:"id"`
	ModbusID  *int     `json:"modbus_id"`
	Name      string   `json:"name"`
	Aggregate bool     `json:"aggregate"`
	Voltage   *float64 `json:"voltage"`
	Current   *float64 `json:"current"`
	Wattage   *int     `json:"wattage"`
	SOC       *int     `json:"soc"`
	Status    string   `json:"status"`
	UpdatedAt *string  `json:"updated_at"`
}

// chargerJSON is the solar charge controller as sent to the client. The raw
// Victron codes are decoded to human-readable names here so the display layer
// does not have to duplicate the lookup tables.
type chargerJSON struct {
	ID       int    `json:"id"`
	ModbusID *int   `json:"modbus_id"`
	Name     string `json:"name"`

	BatteryVoltage *float64 `json:"battery_voltage"`
	BatteryCurrent *float64 `json:"battery_current"`

	PVVoltage *float64 `json:"pv_voltage"`
	PVCurrent *float64 `json:"pv_current"`
	PVPower   *float64 `json:"pv_power"`

	YieldToday    *float64 `json:"yield_today"`
	MaxPowerToday *int     `json:"max_power_today"`

	// MaxAmperage is the charger's configured rated output amps, or nil when
	// unconfigured. The client scales the flow animation against it.
	MaxAmperage *float64 `json:"max_amperage"`

	ChargeState     *int    `json:"charge_state"`
	ChargeStateName *string `json:"charge_state_name"`
	MPPMode         *int    `json:"mpp_mode"`
	MPPModeName     *string `json:"mpp_mode_name"`
	ErrorCode       *int    `json:"error_code"`
	ErrorName       *string `json:"error_name"`

	Status    string  `json:"status"`
	UpdatedAt *string `json:"updated_at"`
}

// statusResponse is the whole dashboard payload for one refresh.
type statusResponse struct {
	Shunts        []shuntJSON  `json:"shunts"`
	Charger       *chargerJSON `json:"charger"`
	SOCLowPercent int          `json:"soc_low_percent"`
}

func (s *dashboardServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Config-derived facts are read fresh each request so live device edits are
	// reflected without restarting the server.
	cfg := s.currentConfig()

	shunts, err := s.queryShunts(aggregateIDs(cfg))
	if err != nil {
		s.logger.Error("dashboard: query shunts", "error", err)
		http.Error(w, "failed to read battery status", http.StatusInternalServerError)
		return
	}

	charger, err := s.queryCharger(chargerMaxAmperage(cfg))
	if err != nil {
		s.logger.Error("dashboard: query charger", "error", err)
		http.Error(w, "failed to read charger status", http.StatusInternalServerError)
		return
	}

	writeJSON(w, statusResponse{
		Shunts:        shunts,
		Charger:       charger,
		SOCLowPercent: cfg.SOCLowPercent,
	})
}

// currentConfig returns the config from disk, falling back to the last good
// copy if the read momentarily fails (e.g. caught mid-write, though writes are
// atomic). Successful reads refresh the cache.
func (s *dashboardServer) currentConfig() Config {
	cfg, err := LoadConfig(s.configPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.logger.Warn("dashboard: config read failed; using cached", "error", err)
		return s.lastConfig
	}

	s.lastConfig = cfg
	return cfg
}

// writeJSON encodes v as the JSON body of a 200 response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

func (s *dashboardServer) queryShunts(aggregateIDs map[int]bool) ([]shuntJSON, error) {
	const query = `
SELECT id, modbus_id, name, voltage, current, wattage, soc, status, updated_at
FROM battery_shunt_status
ORDER BY id;`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query battery shunts: %w", err)
	}
	defer rows.Close()

	var shunts []shuntJSON

	for rows.Next() {
		var (
			id        int
			modbusID  sql.NullInt64
			name      string
			voltage   sql.NullFloat64
			current   sql.NullFloat64
			wattage   sql.NullInt64
			soc       sql.NullInt64
			status    string
			updatedAt sql.NullString
		)

		if err := rows.Scan(
			&id, &modbusID, &name, &voltage, &current, &wattage, &soc, &status, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan battery shunt: %w", err)
		}

		shunts = append(shunts, shuntJSON{
			ID:        id,
			ModbusID:  nullInt(modbusID),
			Name:      name,
			Aggregate: aggregateIDs[id],
			Voltage:   nullFloat(voltage),
			Current:   nullFloat(current),
			Wattage:   nullInt(wattage),
			SOC:       nullInt(soc),
			Status:    status,
			UpdatedAt: nullString(updatedAt),
		})
	}

	return shunts, rows.Err()
}

func (s *dashboardServer) queryCharger(maxAmperage map[int]float64) (*chargerJSON, error) {
	const query = `
SELECT id, modbus_id, name, battery_voltage, battery_current,
       pv_voltage, pv_current, pv_power, yield_today, max_power_today,
       charge_state, mpp_mode, error_code, status, updated_at
FROM charge_controller_status
ORDER BY id
LIMIT 1;`

	var (
		id             int
		modbusID       sql.NullInt64
		name           string
		batteryVoltage sql.NullFloat64
		batteryCurrent sql.NullFloat64
		pvVoltage      sql.NullFloat64
		pvCurrent      sql.NullFloat64
		pvPower        sql.NullFloat64
		yieldToday     sql.NullFloat64
		maxPowerToday  sql.NullInt64
		chargeState    sql.NullInt64
		mppMode        sql.NullInt64
		errorCode      sql.NullInt64
		status         string
		updatedAt      sql.NullString
	)

	err := s.db.QueryRow(query).Scan(
		&id, &modbusID, &name, &batteryVoltage, &batteryCurrent,
		&pvVoltage, &pvCurrent, &pvPower, &yieldToday, &maxPowerToday,
		&chargeState, &mppMode, &errorCode, &status, &updatedAt,
	)
	if err == sql.ErrNoRows {
		// No charge controller registered is a valid configuration.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query charge controller: %w", err)
	}

	var maxAmps *float64
	if v, ok := maxAmperage[id]; ok {
		maxAmps = &v
	}

	return &chargerJSON{
		ID:              id,
		ModbusID:        nullInt(modbusID),
		Name:            name,
		BatteryVoltage:  nullFloat(batteryVoltage),
		BatteryCurrent:  nullFloat(batteryCurrent),
		PVVoltage:       nullFloat(pvVoltage),
		PVCurrent:       nullFloat(pvCurrent),
		PVPower:         nullFloat(pvPower),
		YieldToday:      nullFloat(yieldToday),
		MaxPowerToday:   nullInt(maxPowerToday),
		MaxAmperage:     maxAmps,
		ChargeState:     nullInt(chargeState),
		ChargeStateName: decodedName(chargeState, chargeStateName),
		MPPMode:         nullInt(mppMode),
		MPPModeName:     decodedName(mppMode, mppModeName),
		ErrorCode:       nullInt(errorCode),
		ErrorName:       decodedName(errorCode, chargerErrorName),
		Status:          status,
		UpdatedAt:       nullString(updatedAt),
	}, nil
}

// decodedName runs a raw Victron code through its lookup table, returning nil
// when the code itself is NULL so the client can tell "unknown" from "not read
// yet".
func decodedName(code sql.NullInt64, decode func(uint16) string) *string {
	if !code.Valid {
		return nil
	}

	name := decode(uint16(code.Int64))
	return &name
}

func nullFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}

	return &n.Float64
}

func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}

	v := int(n.Int64)
	return &v
}

func nullString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}

	return &n.String
}

// ---- Device registry API -------------------------------------------------
//
// These handlers only ever rewrite config.json (atomically, serialised by
// writeMu). The poll loop applies the change on its next cycle: it rebuilds the
// in-memory registry and deletes the status rows of any removed devices. No
// device/Modbus state is touched here, so there is nothing to race on.

func (s *dashboardServer) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices := s.currentConfig().Devices
	if devices == nil {
		devices = []DeviceConfig{}
	}

	writeJSON(w, devices)
}

func (s *dashboardServer) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	var d DeviceConfig
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "invalid device JSON", http.StatusBadRequest)
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg, ok := s.loadForWrite(w)
	if !ok {
		return
	}

	// The server assigns the ID from the monotonic counter and advances it, so
	// IDs are never reused (which would mix a new device into a deleted one's
	// history).
	d.ID = nextDeviceID(cfg)
	cfg.NextDeviceID = d.ID + 1
	cfg.Devices = append(cfg.Devices, d)

	if !s.persist(w, cfg) {
		return
	}

	writeJSON(w, d)
}

func (s *dashboardServer) handleUpdateDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	var d DeviceConfig
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "invalid device JSON", http.StatusBadRequest)
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg, ok := s.loadForWrite(w)
	if !ok {
		return
	}

	idx := -1
	for i := range cfg.Devices {
		if cfg.Devices[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	// ID and device type are fixed on edit; only the mutable fields change.
	// Keeping the type immutable avoids a device having to move status tables.
	d.ID = id
	d.DeviceType = cfg.Devices[idx].DeviceType
	cfg.Devices[idx] = d

	if !s.persist(w, cfg) {
		return
	}

	writeJSON(w, d)
}

func (s *dashboardServer) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg, ok := s.loadForWrite(w)
	if !ok {
		return
	}

	kept := make([]DeviceConfig, 0, len(cfg.Devices))
	found := false
	for _, d := range cfg.Devices {
		if d.ID == id {
			found = true
			continue
		}
		kept = append(kept, d)
	}
	if !found {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	cfg.Devices = kept

	// persist may reject deleting the last device (validate requires >= 1).
	if !s.persist(w, cfg) {
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// settingsBody is the display-settings payload exchanged with the client.
type settingsBody struct {
	Background string `json:"background"`
}

func (s *dashboardServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, settingsBody{Background: s.currentConfig().Background})
}

func (s *dashboardServer) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body settingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid settings JSON", http.StatusBadRequest)
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg, ok := s.loadForWrite(w)
	if !ok {
		return
	}

	cfg.Background = body.Background
	if !s.persist(w, cfg) { // validate() rejects unknown background values
		return
	}

	writeJSON(w, settingsBody{Background: cfg.Background})
}

// loadForWrite reads the authoritative config from disk for a read-modify-write
// cycle, writing a 500 and returning ok=false on failure.
func (s *dashboardServer) loadForWrite(w http.ResponseWriter) (Config, bool) {
	cfg, err := LoadConfig(s.configPath)
	if err != nil {
		s.logger.Error("dashboard: load config for write", "error", err)
		http.Error(w, "failed to read configuration", http.StatusInternalServerError)
		return Config{}, false
	}

	return cfg, true
}

// persist validates and atomically writes cfg, mapping a validation failure to
// 400 (the client's fault, message shown in the form) and a write failure to
// 500. Returns true only when the save succeeded.
func (s *dashboardServer) persist(w http.ResponseWriter, cfg Config) bool {
	if err := cfg.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}

	if err := SaveConfig(s.configPath, cfg); err != nil {
		s.logger.Error("dashboard: save config", "error", err)
		http.Error(w, "failed to save configuration", http.StatusInternalServerError)
		return false
	}

	return true
}

// shutdownDashboard stops the HTTP server, giving in-flight requests a short
// grace period to finish.
func shutdownDashboard(srv *http.Server, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("failed to shut down dashboard", "error", err)
	} else {
		logger.Info("dashboard stopped")
	}
}
