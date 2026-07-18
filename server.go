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
	"time"
)

// webFiles holds the dashboard's static assets. Embedding them keeps the
// collector a single deployable binary: there are no loose files to lose or
// keep in sync on the target machine.
//
//go:embed web/solar_dashboard.html web/style.css web/dashboard.js web/starfield.js
var webFiles embed.FS

// dashboardServer serves the read-only web dashboard: the static page and a
// single JSON endpoint that reflects the current-status tables. It only reads
// the database; all writes remain the poll loop's job.
type dashboardServer struct {
	db     *sql.DB
	logger *slog.Logger

	// aggregateIDs marks which shunt rows are the pool aggregate rather than an
	// individual bank. The database does not record this (it is a config fact),
	// so the server carries it to label the JSON for the client.
	aggregateIDs map[int]bool

	// maxAmperage holds each charger's rated output amps, keyed by device ID.
	// It is a config fact (not stored in the database) that the client uses to
	// scale the flow animation. Chargers without a configured value are absent.
	maxAmperage map[int]float64

	// socLowPercent is the pool-wide "low" SOC threshold the client uses to
	// colour the ring (config fact, not stored in the database).
	socLowPercent int
}

// StartDashboard builds the HTTP server, begins listening in the background,
// and returns it so the caller can shut it down. A failure to bind is fatal to
// the dashboard but not to the collector, so it is logged rather than returned.
func StartDashboard(logger *slog.Logger, db *sql.DB, cfg Config) *http.Server {
	handler := &dashboardServer{
		db:            db,
		logger:        logger,
		aggregateIDs:  aggregateIDs(cfg),
		maxAmperage:   chargerMaxAmperage(cfg),
		socLowPercent: cfg.SOCLowPercent,
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

// aggregateIDs returns the set of device IDs that are the pool aggregate shunt.
func aggregateIDs(cfg Config) map[int]bool {
	ids := make(map[int]bool)
	for _, d := range cfg.Devices {
		if d.DeviceType == DeviceTypeShunt && d.Aggregate {
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

	// Serve the embedded assets by name, mapping "/" to the dashboard page.
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		// The sub-filesystem is built from a compile-time embed path, so this
		// can only fail if the embed directive and this string disagree.
		panic(fmt.Sprintf("web assets not embedded: %v", err))
	}

	fileServer := http.FileServer(http.FS(static))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			r.URL.Path = "/solar_dashboard.html"
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
	shunts, err := s.queryShunts()
	if err != nil {
		s.logger.Error("dashboard: query shunts", "error", err)
		http.Error(w, "failed to read battery status", http.StatusInternalServerError)
		return
	}

	charger, err := s.queryCharger()
	if err != nil {
		s.logger.Error("dashboard: query charger", "error", err)
		http.Error(w, "failed to read charger status", http.StatusInternalServerError)
		return
	}

	payload := statusResponse{
		Shunts:        shunts,
		Charger:       charger,
		SOCLowPercent: s.socLowPercent,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.logger.Error("dashboard: encode status", "error", err)
	}
}

func (s *dashboardServer) queryShunts() ([]shuntJSON, error) {
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
			Aggregate: s.aggregateIDs[id],
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

func (s *dashboardServer) queryCharger() (*chargerJSON, error) {
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

	var maxAmperage *float64
	if v, ok := s.maxAmperage[id]; ok {
		maxAmperage = &v
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
		MaxAmperage:     maxAmperage,
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
