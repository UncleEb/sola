package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if len(cfg.Devices) != 1 || cfg.Devices[0].DeviceType != DeviceTypeSystem {
		t.Fatalf("unexpected default devices: %+v", cfg.Devices)
	}

	if created2, err := ensureDefaultConfig(path); err != nil || created2 {
		t.Fatalf("second call should be a no-op: created=%v err=%v", created2, err)
	}
}

// TestDefaultConfigIsValid guards against a default that would fail validation
// (which would make SaveConfig in ensureDefaultConfig refuse to write it).
func TestDefaultConfigIsValid(t *testing.T) {
	if err := defaultConfig().validate(); err != nil {
		t.Fatalf("defaultConfig() is not valid: %v", err)
	}
}
