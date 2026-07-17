package main

import (
	"context"
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

	disabledUnitID = -1

	allBanksUnitID        = 100
	allBanksStartAddress  = 840
	allBanksRegisterCount = 4

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
	Name   string
	UnitID int

	Voltage float64
	Current float64
	Power   int16
}

type SolarCharger struct {
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

	banks := []BatteryBank{
		{Name: "Bank 1", UnitID: disabledUnitID},
		{Name: "Bank 2", UnitID: disabledUnitID},
		{Name: "Bank 3", UnitID: 235},
		{Name: "Bank 4", UnitID: 233},
		{Name: "Bank 5", UnitID: 236},
	}

	solarCharger := SolarCharger{
		Name:   "PV Charger",
		UnitID: solarChargerUnitID,
	}

	pollAndPrint(logger, client, banks, &solarCharger)

	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pollAndPrint(logger, client, banks, &solarCharger)

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

			logger.Info("Victron collector stopped")
			return
		}
	}
}

func pollAndPrint(
	logger *slog.Logger,
	client *modbus.ModbusClient,
	banks []BatteryBank,
	solarCharger *SolarCharger,
) {
	allBanks, err := readAllBanks(client)
	if err != nil {
		logger.Error(
			"failed to read All Banks registers",
			"unit_id", allBanksUnitID,
			"error", err,
		)
	} else {
		printAllBanks(allBanks)
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
			continue
		}

		printBatteryBank(banks[i])
	}

	if err := readSolarCharger(client, solarCharger); err != nil {
		logger.Error(
			"failed to read solar charger",
			"charger", solarCharger.Name,
			"unit_id", solarCharger.UnitID,
			"error", err,
		)
	} else {
		printSolarCharger(*solarCharger)
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

	return AllBanksReading{
		Voltage: float64(registers[0]) * 0.1,
		Current: float64(int16(registers[1])) * 0.1,
		Power:   int16(registers[2]),
		SOC:     registers[3],
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
