package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// discardLogger is a no-op logger for tests that call functions requiring one.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEnsureDefaultConfigBootstraps covers the fresh-install path: an empty
// config directory (e.g. a newly mounted Docker volume) must get a valid,
// loadable default written, the parent directory created if needed, and a
// second call must be a no-op so an existing config is never clobbered.
func TestEnsureDefaultConfigBootstraps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json") // nested: must be created

	created, err := ensureDefaultConfig(path)
	if err != nil || !created {
		t.Fatalf("first call: created=%v err=%v", created, err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("default config failed to load/validate: %v", err)
	}

	// A fresh install ships empty: no devices and no sources. The dashboard
	// comes up with a welcome/empty state inviting the user to add them.
	if len(cfg.Devices) != 0 {
		t.Fatalf("expected no default devices, got %d: %+v", len(cfg.Devices), cfg.Devices)
	}
	if len(cfg.Sources) != 0 {
		t.Fatalf("expected no default sources, got %d: %+v", len(cfg.Sources), cfg.Sources)
	}

	if created2, err := ensureDefaultConfig(path); err != nil || created2 {
		t.Fatalf("second call should be a no-op: created=%v err=%v", created2, err)
	}
}

// TestMigrateLegacyConfig covers the in-memory upgrade of a pre-sources config:
// modbus_url becomes a "victron" modbus source, each madbus[] entry becomes a
// madbus source, source-less (Modbus) devices are repointed at "victron", and
// the legacy fields are cleared.
func TestMigrateLegacyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
        "poll_interval_seconds": 5,
        "database_path": "sola.db",
        "modbus_url": "tcp://10.0.0.5:502",
        "madbus": [{"name": "garage", "url": "http://10.0.0.9:8090"}],
        "next_device_id": 3,
        "devices": [
            {"id": 1, "name": "System", "device_type": "system", "modbus_unit": 100},
            {"id": 2, "name": "Meter", "device_type": "energy_meter", "source": "garage", "madbus_id": "meter-1"}
        ]
    }`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("legacy config failed to load/migrate: %v", err)
	}

	if len(cfg.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d: %+v", len(cfg.Sources), cfg.Sources)
	}
	if s := cfg.Sources[0]; s.Name != "victron" || s.Type != SourceTypeModbus || s.URL != "tcp://10.0.0.5:502" {
		t.Fatalf("victron source not migrated correctly: %+v", s)
	}
	if s := cfg.Sources[1]; s.Name != "garage" || s.Type != SourceTypeMadbus || s.URL != "http://10.0.0.9:8090" {
		t.Fatalf("madbus source not migrated correctly: %+v", s)
	}

	if cfg.Devices[0].Source != "victron" {
		t.Fatalf("modbus device should be repointed at victron, got %q", cfg.Devices[0].Source)
	}
	if cfg.Devices[1].Source != "garage" {
		t.Fatalf("energy meter source should be preserved, got %q", cfg.Devices[1].Source)
	}

	if cfg.ModbusURL != "" || cfg.Madbus != nil {
		t.Fatalf("legacy fields not cleared: modbus_url=%q madbus=%+v", cfg.ModbusURL, cfg.Madbus)
	}
}

// TestMigrateVictronNameCollision verifies the migrated Modbus source dodges a
// name already taken by a legacy madbus instance called "victron".
func TestMigrateVictronNameCollision(t *testing.T) {
	cfg := Config{
		ModbusURL: "tcp://10.0.0.5:502",
		Madbus:    []MadbusInstance{{Name: "victron", URL: "http://10.0.0.9:8090"}},
	}
	cfg.migrate()

	if len(cfg.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d: %+v", len(cfg.Sources), cfg.Sources)
	}
	// The madbus instance keeps "victron"; the modbus source falls back.
	var modbus Source
	for _, s := range cfg.Sources {
		if s.Type == SourceTypeModbus {
			modbus = s
		}
	}
	if modbus.Name == "" || modbus.Name == "victron" {
		t.Fatalf("modbus source should have a disambiguated name, got %q", modbus.Name)
	}
}

// TestMigrateIsIdempotent verifies a second migrate() on an already-migrated
// config is a no-op (it must not duplicate sources or resurrect legacy fields).
func TestMigrateIsIdempotent(t *testing.T) {
	cfg := Config{
		ModbusURL: "tcp://10.0.0.5:502",
		Madbus:    []MadbusInstance{{Name: "garage", URL: "http://10.0.0.9:8090"}},
		Devices:   []DeviceConfig{{ID: 1, Name: "System", DeviceType: DeviceTypeSystem}},
	}
	cfg.migrate()
	first := len(cfg.Sources)
	firstSource := cfg.Devices[0].Source

	cfg.migrate() // second pass

	if len(cfg.Sources) != first {
		t.Fatalf("migrate not idempotent: sources %d -> %d", first, len(cfg.Sources))
	}
	if cfg.Devices[0].Source != firstSource {
		t.Fatalf("device source changed on second migrate: %q -> %q", firstSource, cfg.Devices[0].Source)
	}
}

// TestMigrateConfigFileOnce covers the one-time on-disk rewrite: a legacy file is
// upgraded and persisted once, and a second call leaves the file untouched.
func TestMigrateConfigFileOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
        "poll_interval_seconds": 5,
        "database_path": "sola.db",
        "modbus_url": "tcp://10.0.0.5:502",
        "next_device_id": 2,
        "devices": [
            {"id": 1, "name": "System", "device_type": "system", "modbus_unit": 100}
        ]
    }`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := migrateConfigFileOnce(path, discardLogger()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The rewritten file must carry sources and drop the legacy key.
	var cfg Config
	if err := json.Unmarshal(migrated, &cfg); err != nil {
		t.Fatalf("re-parse migrated file: %v", err)
	}
	if len(cfg.Sources) != 1 || cfg.ModbusURL != "" {
		t.Fatalf("file not migrated on disk: sources=%d modbus_url=%q", len(cfg.Sources), cfg.ModbusURL)
	}

	// Second call is a no-op: the bytes must be identical.
	if err := migrateConfigFileOnce(path, discardLogger()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(migrated) {
		t.Fatalf("second migrate rewrote the file; want no-op")
	}
}

// TestValidateSources exercises the source/device-compatibility rules.
func TestValidateSources(t *testing.T) {
	unit := 100

	// base returns a minimal valid config that each case mutates.
	base := func() Config {
		return Config{
			PollIntervalSeconds: 5,
			DatabasePath:        "sola.db",
			Sources:             []Source{{Name: "victron", Type: SourceTypeModbus, URL: "tcp://10.0.0.5:502"}},
			Devices: []DeviceConfig{
				{ID: 1, Name: "System", DeviceType: DeviceTypeSystem, ModbusUnit: &unit, Source: "victron"},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid modbus only", func(c *Config) {}, false},
		{"valid mixed sources", func(c *Config) {
			c.Sources = append(c.Sources, Source{Name: "garage", Type: SourceTypeMadbus, URL: "http://10.0.0.9:8090"})
			c.Devices = append(c.Devices, DeviceConfig{ID: 2, Name: "Meter", DeviceType: DeviceTypeEnergyMeter, Source: "garage", MadbusID: "meter-1"})
		}, false},
		{"empty source name", func(c *Config) { c.Sources[0].Name = "" }, true},
		{"duplicate source name", func(c *Config) {
			c.Sources = append(c.Sources, Source{Name: "victron", Type: SourceTypeMadbus, URL: "http://x:8090"})
		}, true},
		{"bad source type", func(c *Config) { c.Sources[0].Type = "carrier-pigeon" }, true},
		{"empty source url", func(c *Config) { c.Sources[0].URL = "" }, true},
		{"two modbus sources", func(c *Config) {
			c.Sources = append(c.Sources, Source{Name: "victron2", Type: SourceTypeModbus, URL: "tcp://10.0.0.6:502"})
		}, true},
		{"device references missing source", func(c *Config) { c.Devices[0].Source = "nope" }, true},
		{"device empty source", func(c *Config) { c.Devices[0].Source = "" }, true},
		{"cross-type mismatch: meter on modbus source", func(c *Config) {
			c.Devices = append(c.Devices, DeviceConfig{ID: 2, Name: "Meter", DeviceType: DeviceTypeEnergyMeter, Source: "victron", MadbusID: "meter-1"})
		}, true},
		{"cross-type mismatch: shunt on madbus source", func(c *Config) {
			c.Sources = append(c.Sources, Source{Name: "garage", Type: SourceTypeMadbus, URL: "http://10.0.0.9:8090"})
			c.Devices = append(c.Devices, DeviceConfig{ID: 2, Name: "Shunt", DeviceType: DeviceTypeShunt, Source: "garage"})
		}, true},
		{"modbus device missing unit", func(c *Config) {
			c.Devices = append(c.Devices, DeviceConfig{ID: 2, Name: "Bank", DeviceType: DeviceTypeShunt, Source: "victron"})
		}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			err := cfg.validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestDefaultConfigIsValid guards against a default that would fail validation
// (which would make SaveConfig in ensureDefaultConfig refuse to write it).
func TestDefaultConfigIsValid(t *testing.T) {
	if err := defaultConfig().validate(); err != nil {
		t.Fatalf("defaultConfig() is not valid: %v", err)
	}
}
