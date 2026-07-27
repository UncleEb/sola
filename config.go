package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	DeviceTypeShunt            = "shunt"
	DeviceTypeChargeController = "charge_controller"
	// DeviceTypeSystem is the Venus "System" service (unit 100 by default). It
	// exposes the pool aggregate (voltage/current/power/SOC) that Venus computes
	// across all batteries, using a different register map than a battery shunt.
	// It is an alternative aggregate source for installs without a whole-bank
	// shunt.
	DeviceTypeSystem = "system"
	// DeviceTypeEnergyMeter is an AC energy meter whose readings come from a
	// MadBus instance over HTTP (RS-485 normalized to JSON), not from the Victron
	// Modbus link. Such a device carries a Source (the MadBus instance name) and
	// a MadbusID instead of a ModbusUnit.
	DeviceTypeEnergyMeter = "energy_meter"
)

// Config is the on-disk configuration, holding the deployment-specific values
// that were previously hardcoded. Victron protocol facts (register addresses,
// scale factors) remain code constants, since they are fixed by the device
// type rather than by an installation.
type Config struct {
	PollIntervalSeconds int            `json:"poll_interval_seconds"`
	DatabasePath        string         `json:"database_path"`
	HTTPAddr            string         `json:"http_addr"`                // dashboard listen address; defaults to defaultHTTPAddr
	Debug               bool           `json:"debug"`                    // when true, print each poll's readings to stdout
	SOCLowPercent       int            `json:"soc_low_percent"`          // SOC at/below which the dashboard ring is fully "low" coloured; defaults to defaultSOCLowPercent
	Background          string         `json:"background"`               // dashboard background: none | starfield | warpspeed; defaults to defaultBackground
	HistoryIntervalSec  int            `json:"history_interval_seconds"` // snapshot cadence for the history tables; defaults to defaultHistoryIntervalSec
	NextDeviceID        int            `json:"next_device_id"`           // monotonic ID allocator; only ever increases so IDs are never reused
	Sources             []Source       `json:"sources"`                  // data sources (Victron Modbus + MadBus); a device references one by name via its Source
	Devices             []DeviceConfig `json:"devices"`

	// Legacy fields, kept only so pre-sources configs still parse and can be
	// migrated (see migrate). They are cleared during migration, so a canonical
	// (migrated) config never re-emits them.
	ModbusURL string           `json:"modbus_url,omitempty"` // DEPRECATED: migrated into a "victron" modbus source
	Madbus    []MadbusInstance `json:"madbus,omitempty"`     // DEPRECATED: migrated into type:"madbus" sources
}

// Source is one data source Sola polls: either the Victron Modbus link
// (type "modbus") or a MadBus HTTP normalization service (type "madbus"). A
// device references a source by Name. Sources are managed from the Settings UI,
// so connection endpoints live in config, not in the deployment (env/compose).
type Source struct {
	Name string `json:"name"` // unique key referenced by a device's Source
	Type string `json:"type"` // SourceTypeModbus | SourceTypeMadbus
	URL  string `json:"url"`  // tcp://host:502 (modbus) or http://host:8090 (madbus)
}

// Source types.
const (
	SourceTypeModbus = "modbus"
	SourceTypeMadbus = "madbus"
)

// MadbusInstance is the legacy shape of a MadBus source (pre-sources schema),
// retained only for migration. New configs use Source with Type "madbus".
type MadbusInstance struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Background options for the dashboard.
const (
	BackgroundNone      = "none"
	BackgroundStarfield = "starfield"
	BackgroundWarpspeed = "warpspeed"

	defaultBackground = BackgroundStarfield
)

// defaultHistoryIntervalSec is the history snapshot cadence used when
// history_interval_seconds is omitted.
const defaultHistoryIntervalSec = 15

// defaultHTTPAddr is the dashboard listen address used when http_addr is
// omitted from the config file.
const defaultHTTPAddr = ":8088"

// defaultSOCLowPercent is the "low" SOC threshold used when soc_low_percent is
// omitted. At/below it the dashboard ring is fully the low colour; at 100% it
// is fully the healthy colour, interpolated in between.
const defaultSOCLowPercent = 50

// DeviceConfig describes one device in the registry. ModbusUnit is a pointer so
// a MadBus device's null unit is distinguishable from unit 0; a Modbus device
// must always carry a unit (enforced by validate).
type DeviceConfig struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	DeviceType  string   `json:"device_type"`            // DeviceTypeShunt | DeviceTypeChargeController | DeviceTypeSystem | DeviceTypeEnergyMeter
	ModbusUnit  *int     `json:"modbus_unit"`            // required for Modbus devices; always nil for MadBus-sourced devices. Pointer so a MadBus device's null is distinct from unit 0.
	Aggregate   bool     `json:"aggregate,omitempty"`    // shunt that owns pool SOC
	MaxAmperage *float64 `json:"max_amperage,omitempty"` // charge_controller only: rated output amps, used to scale the dashboard flow animation
	Source      string   `json:"source,omitempty"`       // name of the Source this device is read from
	MadbusID    string   `json:"madbus_id,omitempty"`    // device id within the MadBus source named by Source
}

// configPath returns the path to config.json. The directory is overridable via
// SOLA_CONFIG_DIR so a Docker deployment can mount it as a volume; it
// defaults to the current directory for local development.
func configPath() string {
	dir := os.Getenv("SOLA_CONFIG_DIR")
	if dir == "" {
		dir = "."
	}

	return filepath.Join(dir, "config.json")
}

// LoadConfig reads, parses, and validates the configuration file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Upgrade a legacy (pre-sources) config in memory so every caller sees the
	// canonical schema. This is idempotent and does not write to disk; the
	// one-time on-disk rewrite happens at startup via migrateConfigFileOnce.
	cfg.migrate()

	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}

	// An omitted listen address is not an error; fall back to the default so
	// the dashboard still comes up.
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultHTTPAddr
	}

	// An omitted (zero) low-SOC threshold falls back to the default.
	if cfg.SOCLowPercent == 0 {
		cfg.SOCLowPercent = defaultSOCLowPercent
	}

	// An omitted background falls back to the default (starfield).
	if cfg.Background == "" {
		cfg.Background = defaultBackground
	}

	// A missing/invalid history interval falls back to the default.
	if cfg.HistoryIntervalSec <= 0 {
		cfg.HistoryIntervalSec = defaultHistoryIntervalSec
	}

	return cfg, nil
}

// migrate upgrades a legacy (pre-sources) config to the canonical sources schema
// in memory. It is idempotent: it only acts when Sources is empty, and a
// migrated or fresh config always has at least one source. It builds sources
// from the deprecated ModbusURL/Madbus fields, repoints Victron devices at the
// new "victron" source, and clears the legacy fields so they never round-trip.
func (c *Config) migrate() {
	if len(c.Sources) > 0 {
		return
	}

	victronName := ""
	if c.ModbusURL != "" {
		victronName = c.uniqueSourceName("victron")
		c.Sources = append(c.Sources, Source{Name: victronName, Type: SourceTypeModbus, URL: c.ModbusURL})
	}

	for _, m := range c.Madbus {
		c.Sources = append(c.Sources, Source{Name: m.Name, Type: SourceTypeMadbus, URL: m.URL})
	}

	// Legacy devices with no Source were Victron/Modbus devices; point them at
	// the migrated victron source. Energy meters already carry a madbus name.
	if victronName != "" {
		for i := range c.Devices {
			if c.Devices[i].Source == "" {
				c.Devices[i].Source = victronName
			}
		}
	}

	c.ModbusURL = ""
	c.Madbus = nil
}

// uniqueSourceName returns base, or base with a disambiguating suffix if a
// source (or a legacy madbus instance) already uses that name.
func (c *Config) uniqueSourceName(base string) string {
	taken := func(name string) bool {
		for _, s := range c.Sources {
			if s.Name == name {
				return true
			}
		}
		for _, m := range c.Madbus {
			if m.Name == name {
				return true
			}
		}
		return false
	}

	if !taken(base) {
		return base
	}
	if alt := base + "-modbus"; !taken(alt) {
		return alt
	}
	for i := 2; ; i++ {
		if alt := fmt.Sprintf("%s-%d", base, i); !taken(alt) {
			return alt
		}
	}
}

// migrateConfigFileOnce upgrades a legacy config file to the sources schema on
// disk, exactly once. LoadConfig already migrates in memory on every read; this
// only persists the upgrade so the file a human reads matches reality. It is a
// no-op for an already-migrated or fresh config.
func migrateConfigFileOnce(path string, logger *slog.Logger) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	// Already migrated, or nothing legacy to migrate.
	if len(cfg.Sources) > 0 || (cfg.ModbusURL == "" && len(cfg.Madbus) == 0) {
		return nil
	}

	cfg.migrate()
	if err := SaveConfig(path, cfg); err != nil {
		return fmt.Errorf("persist migrated config: %w", err)
	}

	logger.Info("migrated configuration to the sources schema", "path", path, "sources", len(cfg.Sources))
	return nil
}

// defaultConfig returns a minimal, valid configuration used to bootstrap a
// fresh install (e.g. an empty mounted Docker volume). It has no data sources
// and no devices: rather than guessing at hardware that may not exist, the
// dashboard comes up empty and invites the user to add a data source, then a
// device, from Settings. Everything else is a sensible default.
func defaultConfig() Config {
	return Config{
		PollIntervalSeconds: 5,
		DatabasePath:        "sola.db",
		HTTPAddr:            defaultHTTPAddr,
		SOCLowPercent:       defaultSOCLowPercent,
		Background:          defaultBackground,
		HistoryIntervalSec:  defaultHistoryIntervalSec,
		NextDeviceID:        1,
		Sources:             []Source{},
		Devices:             []DeviceConfig{},
	}
}

// ensureDefaultConfig writes a default config to path when none exists yet, so
// a first run against an empty config directory (a freshly mounted volume) can
// boot instead of failing. It reports whether it created the file; an existing
// config is left untouched.
func ensureDefaultConfig(path string) (created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat config %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("create config dir %s: %w", dir, err)
		}
	}

	if err := SaveConfig(path, defaultConfig()); err != nil {
		return false, err
	}

	return true, nil
}

// SaveConfig validates cfg and writes it to path atomically (temp file in the
// same directory, then rename) so a concurrent reader — the poll loop reloads
// config every cycle — never observes a half-written file. It refuses to write
// a config that would not pass validation, so the file on disk always loads.
func SaveConfig(path string, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // harmless no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	return nil
}

// nextDeviceID returns the ID to assign to a new device. It is the persisted
// monotonic counter, floored to just past any existing ID (so a hand-edited or
// pre-counter config still can't collide). IDs are NEVER reused: because the
// counter only advances, a deleted device's ID is never handed out again, which
// keeps its historical rows unambiguously its own.
//
// The caller must persist cfg.NextDeviceID = returned + 1 so the counter sticks.
func nextDeviceID(cfg Config) int {
	next := cfg.NextDeviceID
	for _, d := range cfg.Devices {
		if d.ID >= next {
			next = d.ID + 1
		}
	}

	return next
}

// validate rejects configurations that could not run correctly, so problems
// surface as a clear message rather than as confusing runtime behavior.
func (c Config) validate() error {
	if c.PollIntervalSeconds <= 0 {
		return fmt.Errorf("poll_interval_seconds must be positive, got %d", c.PollIntervalSeconds)
	}

	if c.DatabasePath == "" {
		return errors.New("database_path is required")
	}

	if c.SOCLowPercent < 0 || c.SOCLowPercent > 100 {
		return fmt.Errorf("soc_low_percent must be between 0 and 100, got %d", c.SOCLowPercent)
	}

	switch c.Background {
	case "", BackgroundNone, BackgroundStarfield, BackgroundWarpspeed:
	default:
		return fmt.Errorf("background must be one of %q, %q, %q; got %q",
			BackgroundNone, BackgroundStarfield, BackgroundWarpspeed, c.Background)
	}

	// Data sources: unique non-empty names, a valid type, and a non-empty URL.
	// This version supports a single Modbus source (multiple is a later change);
	// MadBus sources may be many.
	sources := make(map[string]Source)
	modbusSources := 0
	for _, s := range c.Sources {
		if s.Name == "" {
			return errors.New("source: name is required")
		}
		if _, dup := sources[s.Name]; dup {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		switch s.Type {
		case SourceTypeModbus:
			modbusSources++
		case SourceTypeMadbus:
			// many allowed
		default:
			return fmt.Errorf("source %q: type must be %q or %q, got %q", s.Name, SourceTypeModbus, SourceTypeMadbus, s.Type)
		}
		if s.URL == "" {
			return fmt.Errorf("source %q: url is required", s.Name)
		}
		sources[s.Name] = s
	}
	if modbusSources > 1 {
		return fmt.Errorf("at most one modbus source is supported in this version, found %d", modbusSources)
	}

	// A fresh install has no sources and no devices — the dashboard comes up
	// empty and invites the user to add them. So zero devices is valid; the
	// per-device rules below only apply once devices exist.

	seen := make(map[int]bool)
	aggregates := 0

	for _, d := range c.Devices {
		if d.Name == "" {
			return fmt.Errorf("device %d: name is required", d.ID)
		}

		if seen[d.ID] {
			return fmt.Errorf("duplicate device id %d", d.ID)
		}
		seen[d.ID] = true

		// Every device is read from a declared source, and its type must match
		// the source type: Modbus devices (shunt/charge_controller/system) from a
		// modbus source, energy meters from a madbus source.
		if d.Source == "" {
			return fmt.Errorf("device %d: source is required", d.ID)
		}
		src, ok := sources[d.Source]
		if !ok {
			return fmt.Errorf("device %d: source %q is not declared", d.ID, d.Source)
		}
		wantType := SourceTypeModbus
		if d.DeviceType == DeviceTypeEnergyMeter {
			wantType = SourceTypeMadbus
		}
		if src.Type != wantType {
			return fmt.Errorf("device %d (%s): requires a %q source, but %q is a %q source",
				d.ID, d.DeviceType, wantType, d.Source, src.Type)
		}

		// Every Modbus device must name a unit: a device with no unit couldn't be
		// polled, and a "no port" placeholder just leaves the user guessing whether
		// it's misconfigured or genuinely offline. (Energy meters are MadBus-sourced
		// and carry a madbus_id instead — checked in their case below.)
		if wantType == SourceTypeModbus && d.ModbusUnit == nil {
			return fmt.Errorf("device %d (%s): a Modbus unit ID is required", d.ID, d.DeviceType)
		}

		switch d.DeviceType {
		case DeviceTypeShunt:
			if d.Aggregate {
				aggregates++
			}
			if d.MaxAmperage != nil {
				return fmt.Errorf("device %d: max_amperage is only valid for %q", d.ID, DeviceTypeChargeController)
			}
		case DeviceTypeChargeController:
			if d.Aggregate {
				return fmt.Errorf("device %d: aggregate is only valid for %q", d.ID, DeviceTypeShunt)
			}
			if d.MaxAmperage != nil && *d.MaxAmperage <= 0 {
				return fmt.Errorf("device %d: max_amperage must be positive, got %g", d.ID, *d.MaxAmperage)
			}
		case DeviceTypeSystem:
			// A system device is always the pool aggregate, so it counts toward
			// the single-aggregate limit and does not take the aggregate flag.
			aggregates++
			if d.Aggregate {
				return fmt.Errorf("device %d: the aggregate flag is implicit for %q; do not set it", d.ID, DeviceTypeSystem)
			}
			if d.MaxAmperage != nil {
				return fmt.Errorf("device %d: max_amperage is only valid for %q", d.ID, DeviceTypeChargeController)
			}
		case DeviceTypeEnergyMeter:
			if d.MadbusID == "" {
				return fmt.Errorf("device %d: %q requires madbus_id", d.ID, DeviceTypeEnergyMeter)
			}
			if d.ModbusUnit != nil {
				return fmt.Errorf("device %d: %q is sourced from MadBus, not Modbus; remove modbus_unit", d.ID, DeviceTypeEnergyMeter)
			}
			if d.Aggregate {
				return fmt.Errorf("device %d: aggregate is only valid for %q", d.ID, DeviceTypeShunt)
			}
			if d.MaxAmperage != nil {
				return fmt.Errorf("device %d: max_amperage is only valid for %q", d.ID, DeviceTypeChargeController)
			}
		default:
			return fmt.Errorf("device %d: unknown device_type %q", d.ID, d.DeviceType)
		}
	}

	if aggregates > 1 {
		return fmt.Errorf("at most one aggregate source is allowed (aggregate shunt or system), found %d", aggregates)
	}

	return nil
}
