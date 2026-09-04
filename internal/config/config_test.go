package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.Server.PublicURL = "https://panel.example.test"
	cfg.Storage.DatabasePath = filepath.Join(dir, "xpanel.db")
	cfg.Security.RootKeyFile = filepath.Join(dir, "root.key")
	return cfg
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"valid", func(*Config) {}, ""},
		{"remote xray", func(c *Config) { c.Xray.APIEndpoint = "192.0.2.1:10085" }, "loopback"},
		{"production http", func(c *Config) { c.Server.PublicURL = "http://panel.test" }, "HTTPS"},
		{"bad timezone", func(c *Config) { c.Initial.QuotaTimezone = "Mars/Base" }, "IANA"},
		{"traffic too slow", func(c *Config) { c.Workers.TrafficInterval.Duration = 61 * time.Second }, "60s"},
		{"reconcile too slow", func(c *Config) { c.Workers.ReconcileInterval.Duration = 31 * time.Second }, "30s"},
		{"wrong version", func(c *Config) { c.Xray.SupportedVersion = "latest" }, SupportedXrayVersion},
		{"zero duration", func(c *Config) { c.Xray.RPCTimeout.Duration = 0 }, "positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.edit(&cfg)
			err := cfg.Validate()
			if tt.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error = %v, want safe text containing %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "super-secret") {
				t.Fatal("error leaked a sensitive value")
			}
		})
	}
}

func TestTrafficIntervalWarning(t *testing.T) {
	cfg := validConfig(t)
	cfg.Workers.TrafficInterval.Duration = 6 * time.Second
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "overshoot") {
		t.Fatalf("warnings = %#v", cfg.Warnings)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}
