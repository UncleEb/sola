package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/simonvetter/modbus"
)

const (
	modbusURL  = "tcp://192.168.1.4:502"
	pollPeriod = 5 * time.Second

	dbPath = "victron.db"

	disabledUnitID = -1

	// Stable app-assigned device IDs form a single global registry across all
	// device tables: 1 = aggregate shunt, 2..6 = the five banks (see the banks
	// slice), 7 = the charge controller.
	allBanksDeviceID         = 1
	chargeControllerDeviceID = 7
	allBanksName             = "All Banks"

	// The aggregate shunt (unit 239) is a battery service, so it uses the same
	// register map as the individual banks (258=Power, 259=Voltage,
	// 261=Current) plus SOC at 266 — not the System map at 840.
	allBanksUnitID        = 239
	allBanksStartAddress  = 258
	allBanksRegisterCount = 4
	allBanksSOCAddress    = 266

	bankStartAddress  = 258
	bankRegisterCount = 4

	solarChargerUnitID = 238
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

	client, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     modbusURL,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		logger.Error("failed to create Modbus client", "error", err)
		os.Exit(1)
	}

	if err := client.Open(); err != nil {
		logger.Error(
			"failed to connect to Victron Modbus server",
			"url", modbusURL,
			"error", err,
		)
		os.Exit(1)
	}

	logger.Info(
		"connected to Victron Modbus server",
		"url", modbusURL,
	)

	db, err := OpenDatabase(dbPath)
	if err != nil {
		logger.Error("failed to open database", "path", dbPath, "error", err)
		os.Exit(1)
	}

	if err := createSchema(db); err != nil {
		logger.Error("failed to create database schema", "error", err)
		os.Exit(1)
	}

	logger.Info("database ready", "path", dbPath)

	// All banks are registered, including those not currently wired up. Banks
	// 1 and 2 have no exposed Modbus port (disabledUnitID), so they are never
	// polled and stay offline until hardware is connected.
	banks := []BatteryBank{
		{ID: 2, Name: "Bank 1", UnitID: disabledUnitID},
		{ID: 3, Name: "Bank 2", UnitID: disabledUnitID},
		{ID: 4, Name: "Bank 3", UnitID: 235},
		{ID: 5, Name: "Bank 4", UnitID: 233},
		{ID: 6, Name: "Bank 5", UnitID: 236},
	}

	solarCharger := SolarCharger{
		ID:     chargeControllerDeviceID,
		Name:   "PV Charger",
		UnitID: solarChargerUnitID,
	}

	if err := seedDevices(db, banks, &solarCharger); err != nil {
		logger.Error("failed to seed device registry", "error", err)
		os.Exit(1)
	}

	pollAndPrint(logger, db, client, banks, &solarCharger)

	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pollAndPrint(logger, db, client, banks, &solarCharger)

		case <-ctx.Done():
			logger.Info("shutdown signal received")

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

// seedDevices registers every known device in its status table so that even
// never-polled devices (such as disconnected banks) are visible as offline
// from startup. Existing rows are left untouched.
func seedDevices(db *sql.DB, banks []BatteryBank, charger *SolarCharger) error {
	if err := seedDevice(
		db, tableBatteryShunt, allBanksDeviceID,
		modbusID(allBanksUnitID), allBanksName,
	); err != nil {
		return err
	}

	for _, bank := range banks {
		if err := seedDevice(
			db, tableBatteryShunt, bank.ID, modbusID(bank.UnitID), bank.Name,
		); err != nil {
			return err
		}
	}

	return seedDevice(
		db, tableChargeController, charger.ID,
		modbusID(charger.UnitID), charger.Name,
	)
}

// modbusID converts an in-memory unit ID into a nullable database value,
// treating the disabled sentinel as "no Modbus mapping" (NULL).
func modbusID(unitID int) sql.NullInt64 {
	if unitID == disabledUnitID {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: int64(unitID), Valid: true}
}

func pollAndPrint(
	logger *slog.Logger,
	db *sql.DB,
	client *modbus.ModbusClient,
	banks []BatteryBank,
	solarCharger *SolarCharger,
) {
	// One timestamp for the whole poll so every row updated in this cycle
	// shares the same reading time.
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	allBanks, err := readAllBanks(client)
	if err != nil {
		logger.Error(
			"failed to read All Banks registers",
			"unit_id", allBanksUnitID,
			"error", err,
		)

		if err := markDeviceOffline(db, tableBatteryShunt, allBanksDeviceID); err != nil {
			logger.Error("failed to mark All Banks offline", "error", err)
		}
	} else {
		printAllBanks(allBanks)

		shunt := ShuntStatus{
			ID:       allBanksDeviceID,
			ModbusID: allBanksUnitID,
			Name:     allBanksName,
			Voltage:  allBanks.Voltage,
			Current:  allBanks.Current,
			Wattage:  int(allBanks.Power),
			// The aggregate owns the pool SOC.
			SOC: sql.NullInt64{Int64: int64(allBanks.SOC), Valid: true},
		}

		if err := upsertBatteryShunt(db, shunt, updatedAt); err != nil {
			logger.Error("failed to store All Banks reading", "error", err)
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

		printBatteryBank(banks[i])

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
	}

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
		printSolarCharger(*solarCharger)

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
	}

	fmt.Println()
}

func readAllBanks(
	client *modbus.ModbusClient,
) (AllBanksReading, error) {
	client.SetUnitId(allBanksUnitID)

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

func printAllBanks(reading AllBanksReading) {
	fmt.Printf(
		"%s | All Banks  | Voltage: %.1f V | Current: %.1f A | Power: %d W | SOC: %d%%\n",
		currentTime(),
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
