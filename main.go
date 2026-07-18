package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
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

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	// A missing or invalid config at startup is fatal: there is no prior
	// good state to fall back to, and guessing defaults would be worse than
	// a clear error.
	path := configPath()
	cfg, err := LoadConfig(path)
	if err != nil {
		logger.Error("failed to load configuration", "path", path, "error", err)
		os.Exit(1)
	}

	logger.Info("configuration loaded", "path", path, "devices", len(cfg.Devices))

	client, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     cfg.ModbusURL,
		Timeout: modbusTimeout,
	})
	if err != nil {
		logger.Error("failed to create Modbus client", "error", err)
		os.Exit(1)
	}

	if err := client.Open(); err != nil {
		logger.Error(
			"failed to connect to Victron Modbus server",
			"url", cfg.ModbusURL,
			"error", err,
		)
		os.Exit(1)
	}

	logger.Info(
		"connected to Victron Modbus server",
		"url", cfg.ModbusURL,
	)

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

	// The connection and database are established once at startup; changing
	// modbus_url, database_path, or http_addr requires a restart. The device
	// registry, poll interval, and debug flag are all applied live (see the
	// reload below).
	aggregate, banks, charger := buildDevices(cfg)

	// The dashboard reads the current-status tables the poll loop maintains. It
	// uses the startup address; changing http_addr requires a restart.
	dashboard := StartDashboard(logger, db, cfg, path)

	// History snapshots are captured from the poll loop (no separate goroutine)
	// at most once per history interval. The first poll records a snapshot.
	lastHistoryAt := time.Now()
	pollAndStore(logger, db, client, aggregate, banks, charger, cfg.Debug, true)

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
					aggregate, banks, charger = reconcileDevices(logger, db, current, fresh)
				}

				current = fresh
			}

			// Record a history snapshot once per (hot-appliable) history
			// interval. It is a floor: snapshots ride poll cycles, so they can't
			// be closer together than the poll interval.
			recordHistory := time.Since(lastHistoryAt) >= time.Duration(current.HistoryIntervalSec)*time.Second
			if recordHistory {
				lastHistoryAt = time.Now()
			}

			pollAndStore(logger, db, client, aggregate, banks, charger, current.Debug, recordHistory)

		case <-ctx.Done():
			logger.Info("shutdown signal received")

			shutdownDashboard(dashboard, logger)

			if err := client.Close(); err != nil {
				logger.Error(
					"failed to close Modbus connection",
					"error", err,
				)
			} else {
				logger.Info("Modbus connection closed")
			}

			if err := db.Close(); err != nil {
				logger.Error("failed to close database", "error", err)
			} else {
				logger.Info("database closed")
			}

			logger.Info("Victron collector stopped")
			return
		}
	}
}

// buildDevices turns the configured device list into the in-memory structures
// the poll loop uses: the single aggregate shunt (nil if none), the individual
// banks, and the charge controller (nil if none). validate() has already
// guaranteed device types are valid and at most one aggregate exists.
func buildDevices(cfg Config) (aggregate *BatteryBank, banks []BatteryBank, charger *SolarCharger) {
	for _, d := range cfg.Devices {
		switch d.DeviceType {
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

	return aggregate, banks, charger
}

// reconcileDevices applies a changed device list live: it deletes the status
// rows of devices that were removed, refreshes/seeds identities for the current
// set, and returns the rebuilt in-memory registry. It runs in the poll loop so
// no other goroutine touches device state. Row/seed failures are logged but not
// fatal — the collector keeps running with whatever succeeded.
func reconcileDevices(logger *slog.Logger, db *sql.DB, old, fresh Config) (*BatteryBank, []BatteryBank, *SolarCharger) {
	freshIDs := make(map[int]bool, len(fresh.Devices))
	for _, d := range fresh.Devices {
		freshIDs[d.ID] = true
	}

	for _, d := range old.Devices {
		if freshIDs[d.ID] {
			continue
		}

		table := tableBatteryShunt
		if d.DeviceType == DeviceTypeChargeController {
			table = tableChargeController
		}

		if err := deleteDevice(db, table, d.ID); err != nil {
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

// seedDevices registers every configured device in its status table so that
// even never-polled devices (such as disconnected banks) are visible as
// offline from startup. Existing rows are left untouched.
func seedDevices(db *sql.DB, cfg Config) error {
	for _, d := range cfg.Devices {
		table := tableBatteryShunt
		if d.DeviceType == DeviceTypeChargeController {
			table = tableChargeController
		}

		if err := seedDevice(
			db, table, d.ID, modbusID(unitOrDisabled(d.ModbusUnit)), d.Name,
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

func pollAndStore(
	logger *slog.Logger,
	db *sql.DB,
	client *modbus.ModbusClient,
	aggregate *BatteryBank,
	banks []BatteryBank,
	solarCharger *SolarCharger,
	debug bool,
	recordHistory bool,
) {
	// One timestamp for the whole poll so every row updated in this cycle
	// shares the same reading time.
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	if aggregate != nil {
		// The aggregate reads from the System register map when sourced from the
		// Venus System service, and from the battery-service map otherwise.
		readAggregate := readAllBanks
		if aggregate.System {
			readAggregate = readSystem
		}

		allBanks, err := readAggregate(client, aggregate.UnitID)
		if err != nil {
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

		if err := readBatteryBank(client, &banks[i]); err != nil {
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
		if err := readSolarCharger(client, solarCharger); err != nil {
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
