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
)

// Config is the on-disk configuration, holding the deployment-specific values
// that were previously hardcoded. Victron protocol facts (register addresses,
// scale factors) remain code constants, since they are fixed by the device
// type rather than by an installation.
type Config struct {
	ModbusURL           string         `json:"modbus_url"`
	PollIntervalSeconds int            `json:"poll_interval_seconds"`
	DatabasePath        string         `json:"database_path"`
	HTTPAddr            string         `json:"http_addr"` // dashboard listen address; defaults to defaultHTTPAddr
	Debug               bool           `json:"debug"`     // when true, print each poll's readings to stdout
	Devices             []DeviceConfig `json:"devices"`
}

// defaultHTTPAddr is the dashboard listen address used when http_addr is
// omitted from the config file.
const defaultHTTPAddr = ":8088"

// DeviceConfig describes one device in the registry. ModbusUnit is a pointer so
// that a null in the file (a device with no exposed Modbus port) is
// distinguishable from unit 0.
type DeviceConfig struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	DeviceType  string   `json:"device_type"`  // DeviceTypeShunt | DeviceTypeChargeController
	ModbusUnit  *int     `json:"modbus_unit"`  // nil = no exposed port
	Aggregate   bool     `json:"aggregate"`    // shunt that owns pool SOC
	MaxAmperage *float64 `json:"max_amperage"` // charge_controller only: rated output amps, used to scale the dashboard flow animation
}

// configPath returns the path to config.json. The directory is overridable via
// VICTRON_CONFIG_DIR so a Docker deployment can mount it as a volume; it
// defaults to the current directory for local development.
func configPath() string {
	dir := os.Getenv("VICTRON_CONFIG_DIR")
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

	return cfg, nil
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
		default:
			return fmt.Errorf("device %d: unknown device_type %q", d.ID, d.DeviceType)
		}
	}

	if aggregates > 1 {
		return fmt.Errorf("at most one aggregate shunt is allowed, found %d", aggregates)
	}

	return nil
}
