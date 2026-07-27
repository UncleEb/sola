package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const (
	tableBatteryShunt     = "battery_shunt_status"
	tableChargeController = "charge_controller_status"
	tableEnergyMeter      = "energy_meter_status"
)

// ShuntStatus is a single current-status snapshot for one battery shunt,
// ready to be written to the battery_shunt_status table.
//
// SOC is only populated for the aggregate shunt that owns the pool state of
// charge. For an individual bank it stays zero-valued (Valid: false) and is
// stored as SQL NULL, which is the deliberate signal that the bank is not the
// source of truth for SOC.
type ShuntStatus struct {
	ID       int
	ModbusID int
	Name     string

	Voltage float64
	Current float64
	Wattage int

	SOC sql.NullInt64
}

// OpenDatabase opens (creating it if needed) the SQLite database at path and
// verifies the connection.
//
// busy_timeout makes a briefly-locked database wait rather than fail
// immediately, which matters once a future HTTP API reads while we write. WAL
// journaling lets those readers run concurrently with the poller's writes.
func OpenDatabase(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return db, nil
}

// createSchema creates the tables if they do not already exist. It is safe to
// run on every startup.
func createSchema(db *sql.DB) error {
	// modbus_id is nullable: a registered device with no exposed Modbus port
	// (e.g. a disconnected bank) has no mapping. updated_at is nullable: it
	// records the last successful reading, so a never-read device is NULL.
	// status is 'online' after a successful read and 'offline' otherwise.
	const schema = `
CREATE TABLE IF NOT EXISTS battery_shunt_status (
    id         INTEGER PRIMARY KEY,
    modbus_id  INTEGER,
    name       TEXT    NOT NULL,
    voltage    REAL,
    current    REAL,
    wattage    INTEGER,
    soc        INTEGER,
    status     TEXT    NOT NULL DEFAULT 'offline',
    updated_at TEXT
);

CREATE TABLE IF NOT EXISTS charge_controller_status (
    id              INTEGER PRIMARY KEY,
    modbus_id       INTEGER,
    name            TEXT    NOT NULL,
    battery_voltage REAL,
    battery_current REAL,
    pv_voltage      REAL,
    pv_current      REAL,
    pv_power        REAL,
    yield_today     REAL,
    max_power_today INTEGER,
    charge_state    INTEGER,
    mpp_mode        INTEGER,
    error_code      INTEGER,
    status          TEXT    NOT NULL DEFAULT 'offline',
    updated_at      TEXT
);

-- History: one row per device per snapshot (measurements only; identity lives
-- in the status/registry tables). A row exists only for a successful read, so
-- gaps are genuine offline periods. The composite PRIMARY KEY doubles as the
-- (device_id, ts) index that range/graph queries scan; WITHOUT ROWID stores the
-- rows clustered in that order for compact, fast reads. Rows are keyed by the
-- never-reused device_id, so a deleted device's history is never mixed with a
-- later device's.
CREATE TABLE IF NOT EXISTS battery_shunt_history (
    device_id INTEGER NOT NULL,
    ts        TEXT    NOT NULL,
    voltage   REAL,
    current   REAL,
    wattage   INTEGER,
    soc       INTEGER,
    PRIMARY KEY (device_id, ts)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS charge_controller_history (
    device_id       INTEGER NOT NULL,
    ts              TEXT    NOT NULL,
    battery_voltage REAL,
    battery_current REAL,
    pv_voltage      REAL,
    pv_current      REAL,
    pv_power        REAL,
    yield_today     REAL,
    max_power_today INTEGER,
    charge_state    INTEGER,
    mpp_mode        INTEGER,
    error_code      INTEGER,
    PRIMARY KEY (device_id, ts)
) WITHOUT ROWID;

-- AC energy meter, sourced from a MadBus instance over HTTP. Identity is the
-- MadBus (source, madbus_id) pair rather than a Modbus unit. Metric columns are
-- nullable: they are SQL NULL when the meter is offline or a value is stale/
-- absent, mirroring how a never-read Victron device carries NULLs. status is
-- 'online', 'stale' (values present but MadBus flagged them stale), or 'offline'.
CREATE TABLE IF NOT EXISTS energy_meter_status (
    id             INTEGER PRIMARY KEY,
    source         TEXT    NOT NULL,
    madbus_id      TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    voltage        REAL,
    current        REAL,
    frequency      REAL,
    power          REAL,
    apparent_power REAL,
    reactive_power REAL,
    power_factor   REAL,
    energy_import  REAL,
    energy_export  REAL,
    energy_total   REAL,
    status         TEXT    NOT NULL DEFAULT 'offline',
    updated_at     TEXT
);

CREATE TABLE IF NOT EXISTS energy_meter_history (
    device_id    INTEGER NOT NULL,
    ts           TEXT    NOT NULL,
    voltage      REAL,
    current      REAL,
    power        REAL,
    frequency    REAL,
    power_factor REAL,
    energy_total REAL,
    PRIMARY KEY (device_id, ts)
) WITHOUT ROWID;`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	return nil
}

// upsertBatteryShunt writes the latest reading for one shunt, keeping exactly
// one current row per device ID. The first poll inserts the row; every poll
// after updates it in place.
func upsertBatteryShunt(db *sql.DB, s ShuntStatus, updatedAt string) error {
	// A successful reading is by definition online.
	const query = `
INSERT INTO battery_shunt_status
    (id, modbus_id, name, voltage, current, wattage, soc, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'online', ?)
ON CONFLICT(id) DO UPDATE SET
    modbus_id  = excluded.modbus_id,
    name       = excluded.name,
    voltage    = excluded.voltage,
    current    = excluded.current,
    wattage    = excluded.wattage,
    soc        = excluded.soc,
    status     = excluded.status,
    updated_at = excluded.updated_at;`

	_, err := db.Exec(
		query,
		s.ID, s.ModbusID, s.Name,
		s.Voltage, s.Current, s.Wattage, s.SOC,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert battery shunt %q: %w", s.Name, err)
	}

	return nil
}

// insertShuntHistory appends one historical snapshot for a battery shunt. OR
// IGNORE makes it a no-op if a row already exists for this (device, ts) — e.g.
// two snapshots landing in the same second across a restart.
func insertShuntHistory(db *sql.DB, s ShuntStatus, ts string) error {
	const query = `
INSERT OR IGNORE INTO battery_shunt_history
    (device_id, ts, voltage, current, wattage, soc)
VALUES (?, ?, ?, ?, ?, ?);`

	_, err := db.Exec(query, s.ID, ts, s.Voltage, s.Current, s.Wattage, s.SOC)
	if err != nil {
		return fmt.Errorf("insert shunt history %d: %w", s.ID, err)
	}

	return nil
}

// ChargeControllerStatus is a single current-status snapshot for one solar
// charge controller, ready to be written to the charge_controller_status
// table. The charge_state, mpp_mode, and error_code fields store the raw
// Victron codes; decoding to human-readable text is a display concern.
type ChargeControllerStatus struct {
	ID       int
	ModbusID int
	Name     string

	BatteryVoltage float64
	BatteryCurrent float64

	PVVoltage float64
	PVCurrent float64
	PVPower   float64

	YieldToday    float64
	MaxPowerToday int

	ChargeState int
	MPPMode     int
	ErrorCode   int
}

// upsertChargeController writes the latest reading for one charge controller,
// keeping exactly one current row per device ID.
func upsertChargeController(db *sql.DB, c ChargeControllerStatus, updatedAt string) error {
	const query = `
INSERT INTO charge_controller_status
    (id, modbus_id, name, battery_voltage, battery_current,
     pv_voltage, pv_current, pv_power, yield_today, max_power_today,
     charge_state, mpp_mode, error_code, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'online', ?)
ON CONFLICT(id) DO UPDATE SET
    modbus_id       = excluded.modbus_id,
    name            = excluded.name,
    battery_voltage = excluded.battery_voltage,
    battery_current = excluded.battery_current,
    pv_voltage      = excluded.pv_voltage,
    pv_current      = excluded.pv_current,
    pv_power        = excluded.pv_power,
    yield_today     = excluded.yield_today,
    max_power_today = excluded.max_power_today,
    charge_state    = excluded.charge_state,
    mpp_mode        = excluded.mpp_mode,
    error_code      = excluded.error_code,
    status          = excluded.status,
    updated_at      = excluded.updated_at;`

	_, err := db.Exec(
		query,
		c.ID, c.ModbusID, c.Name,
		c.BatteryVoltage, c.BatteryCurrent,
		c.PVVoltage, c.PVCurrent, c.PVPower, c.YieldToday, c.MaxPowerToday,
		c.ChargeState, c.MPPMode, c.ErrorCode,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert charge controller %q: %w", c.Name, err)
	}

	return nil
}

// insertChargeControllerHistory appends one historical snapshot for a charge
// controller. See insertShuntHistory for the OR IGNORE rationale.
func insertChargeControllerHistory(db *sql.DB, c ChargeControllerStatus, ts string) error {
	const query = `
INSERT OR IGNORE INTO charge_controller_history
    (device_id, ts, battery_voltage, battery_current, pv_voltage, pv_current,
     pv_power, yield_today, max_power_today, charge_state, mpp_mode, error_code)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`

	_, err := db.Exec(
		query,
		c.ID, ts,
		c.BatteryVoltage, c.BatteryCurrent,
		c.PVVoltage, c.PVCurrent, c.PVPower, c.YieldToday, c.MaxPowerToday,
		c.ChargeState, c.MPPMode, c.ErrorCode,
	)
	if err != nil {
		return fmt.Errorf("insert charge controller history %d: %w", c.ID, err)
	}

	return nil
}

// EnergyMeterStatus is a single current-status snapshot for one AC energy meter
// sourced from MadBus. Metric fields are sql.NullFloat64 so a value MadBus
// reports as null (offline/stale/absent) is stored as SQL NULL rather than a
// misleading zero. Status is "online" or "stale"; an offline meter is recorded
// via markDeviceOffline, which preserves its last-good readings.
type EnergyMeterStatus struct {
	ID       int
	Source   string
	MadbusID string
	Name     string

	Voltage       sql.NullFloat64
	Current       sql.NullFloat64
	Frequency     sql.NullFloat64
	Power         sql.NullFloat64
	ApparentPower sql.NullFloat64
	ReactivePower sql.NullFloat64
	PowerFactor   sql.NullFloat64
	EnergyImport  sql.NullFloat64
	EnergyExport  sql.NullFloat64
	EnergyTotal   sql.NullFloat64

	Status string // "online" | "stale"
}

// upsertEnergyMeter writes the latest reading for one energy meter, keeping
// exactly one current row per device ID. Unlike the Victron upserts, status is
// explicit (online vs stale): MadBus can serve values it has flagged stale after
// a device drops.
func upsertEnergyMeter(db *sql.DB, m EnergyMeterStatus, updatedAt string) error {
	const query = `
INSERT INTO energy_meter_status
    (id, source, madbus_id, name, voltage, current, frequency, power,
     apparent_power, reactive_power, power_factor,
     energy_import, energy_export, energy_total, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    source         = excluded.source,
    madbus_id      = excluded.madbus_id,
    name           = excluded.name,
    voltage        = excluded.voltage,
    current        = excluded.current,
    frequency      = excluded.frequency,
    power          = excluded.power,
    apparent_power = excluded.apparent_power,
    reactive_power = excluded.reactive_power,
    power_factor   = excluded.power_factor,
    energy_import  = excluded.energy_import,
    energy_export  = excluded.energy_export,
    energy_total   = excluded.energy_total,
    status         = excluded.status,
    updated_at     = excluded.updated_at;`

	_, err := db.Exec(
		query,
		m.ID, m.Source, m.MadbusID, m.Name,
		m.Voltage, m.Current, m.Frequency, m.Power,
		m.ApparentPower, m.ReactivePower, m.PowerFactor,
		m.EnergyImport, m.EnergyExport, m.EnergyTotal,
		m.Status, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert energy meter %q: %w", m.Name, err)
	}

	return nil
}

// insertEnergyMeterHistory appends one historical snapshot for an energy meter
// (the graph-worthy subset of its metrics). See insertShuntHistory for the OR
// IGNORE rationale.
func insertEnergyMeterHistory(db *sql.DB, m EnergyMeterStatus, ts string) error {
	const query = `
INSERT OR IGNORE INTO energy_meter_history
    (device_id, ts, voltage, current, power, frequency, power_factor, energy_total)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);`

	_, err := db.Exec(
		query,
		m.ID, ts,
		m.Voltage, m.Current, m.Power, m.Frequency, m.PowerFactor, m.EnergyTotal,
	)
	if err != nil {
		return fmt.Errorf("insert energy meter history %d: %w", m.ID, err)
	}

	return nil
}

// seedEnergyMeter registers an energy meter's identity row (id, source,
// madbus_id, name). Like seedDevice, a new device is inserted offline with NULL
// readings; an existing row has only its identity refreshed.
func seedEnergyMeter(db *sql.DB, id int, source, madbusID, name string) error {
	const query = `
INSERT INTO energy_meter_status (id, source, madbus_id, name) VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    source    = excluded.source,
    madbus_id = excluded.madbus_id,
    name      = excluded.name;`

	if _, err := db.Exec(query, id, source, madbusID, name); err != nil {
		return fmt.Errorf("seed energy meter %d: %w", id, err)
	}

	return nil
}

// seedDevice registers a device's identity row. A new device is inserted with
// status at its 'offline' default and reading fields NULL. An existing row has
// only its identity (name, modbus_id) refreshed — status and readings are left
// intact — so live config edits (a rename, a Modbus-unit change) are reflected
// even for devices that are never polled, such as disconnected banks.
// modbusID is NULL for a device with no exposed Modbus port.
func seedDevice(db *sql.DB, table string, id int, modbusID sql.NullInt64, name string) error {
	query := fmt.Sprintf(`
INSERT INTO %s (id, modbus_id, name) VALUES (?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name      = excluded.name,
    modbus_id = excluded.modbus_id;`, table)

	if _, err := db.Exec(query, id, modbusID, name); err != nil {
		return fmt.Errorf("seed device %d in %s: %w", id, table, err)
	}

	return nil
}

// deleteDevice removes a device's status row entirely. It is used when a device
// is removed from the configuration so it disappears from the dashboard rather
// than lingering with its last reading.
func deleteDevice(db *sql.DB, table string, id int) error {
	query := fmt.Sprintf(`DELETE FROM %s WHERE id = ?;`, table)

	if _, err := db.Exec(query, id); err != nil {
		return fmt.Errorf("delete device %d from %s: %w", id, table, err)
	}

	return nil
}

// markDeviceOffline flags a device offline after a failed read, leaving its
// last-good reading and updated_at untouched so you can see when it was last
// healthy.
func markDeviceOffline(db *sql.DB, table string, id int) error {
	query := fmt.Sprintf(`UPDATE %s SET status = 'offline' WHERE id = ?;`, table)

	if _, err := db.Exec(query, id); err != nil {
		return fmt.Errorf("mark device %d offline in %s: %w", id, table, err)
	}

	return nil
}
