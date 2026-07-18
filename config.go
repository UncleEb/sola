package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
)

// Config is the on-disk configuration, holding the deployment-specific values
// that were previously hardcoded. Victron protocol facts (register addresses,
// scale factors) remain code constants, since they are fixed by the device
// type rather than by an installation.
type Config struct {
	ModbusURL           string         `json:"modbus_url"`
	PollIntervalSeconds int            `json:"poll_interval_seconds"`
	DatabasePath        string         `json:"database_path"`
	HTTPAddr            string         `json:"http_addr"`                // dashboard listen address; defaults to defaultHTTPAddr
	Debug               bool           `json:"debug"`                    // when true, print each poll's readings to stdout
	SOCLowPercent       int            `json:"soc_low_percent"`          // SOC at/below which the dashboard ring is fully "low" coloured; defaults to defaultSOCLowPercent
	Background          string         `json:"background"`               // dashboard background: none | starfield | warpspeed; defaults to defaultBackground
	HistoryIntervalSec  int            `json:"history_interval_seconds"` // snapshot cadence for the history tables; defaults to defaultHistoryIntervalSec
	NextDeviceID        int            `json:"next_device_id"`           // monotonic ID allocator; only ever increases so IDs are never reused
	Devices             []DeviceConfig `json:"devices"`
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
// that a null in the file (a device with no exposed Modbus port) is
// distinguishable from unit 0.
type DeviceConfig struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	DeviceType  string   `json:"device_type"`            // DeviceTypeShunt | DeviceTypeChargeController
	ModbusUnit  *int     `json:"modbus_unit"`            // nil = no exposed port
	Aggregate   bool     `json:"aggregate,omitempty"`    // shunt that owns pool SOC
	MaxAmperage *float64 `json:"max_amperage,omitempty"` // charge_controller only: rated output amps, used to scale the dashboard flow animation
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

// defaultConfig returns a minimal, valid configuration used to bootstrap a
// fresh install (e.g. an empty mounted Docker volume). It is deliberately
// generic: a placeholder Modbus URL — overridden by the MODBUS_URL env var or
// by editing config.json — and a single System aggregate device on the Venus
// default unit 100, so the dashboard comes up and can be tailored from there.
func defaultConfig() Config {
	unit := 100
	return Config{
		ModbusURL:           "tcp://192.168.1.100:502",
		PollIntervalSeconds: 5,
		DatabasePath:        "sola.db",
		HTTPAddr:            defaultHTTPAddr,
		SOCLowPercent:       defaultSOCLowPercent,
		Background:          defaultBackground,
		HistoryIntervalSec:  defaultHistoryIntervalSec,
		NextDeviceID:        2,
		Devices: []DeviceConfig{
			{ID: 1, Name: "Battery Pool", DeviceType: DeviceTypeSystem, ModbusUnit: &unit},
		},
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
	if c.ModbusURL == "" {
		return errors.New("modbus_url is required")
	}

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

	if len(c.Devices) == 0 {
		return errors.New("at least one device is required")
	}

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
		default:
			return fmt.Errorf("device %d: unknown device_type %q", d.ID, d.DeviceType)
		}
	}

	if aggregates > 1 {
		return fmt.Errorf("at most one aggregate source is allowed (aggregate shunt or system), found %d", aggregates)
	}

	return nil
}
