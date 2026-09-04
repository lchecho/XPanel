package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const SupportedXrayVersion = "v26.3.27"

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("duration must be a string")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return errors.New("duration must be positive Go duration syntax")
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

type Config struct {
	Server   ServerConfig   `json:"server"`
	Storage  StorageConfig  `json:"storage"`
	Security SecurityConfig `json:"security"`
	Xray     XrayConfig     `json:"xray"`
	Workers  WorkersConfig  `json:"workers"`
	Initial  InitialConfig  `json:"initial"`
	Logging  LoggingConfig  `json:"logging"`
	Warnings []string       `json:"-"`
}

type ServerConfig struct {
	Listen              string   `json:"listen"`
	PublicURL           string   `json:"public_url"`
	InsecureDevelopment bool     `json:"insecure_development"`
	ReadHeaderTimeout   Duration `json:"read_header_timeout"`
	RequestTimeout      Duration `json:"request_timeout"`
	ShutdownTimeout     Duration `json:"shutdown_timeout"`
}

type StorageConfig struct {
	DatabasePath string   `json:"database_path"`
	BusyTimeout  Duration `json:"busy_timeout"`
}

type SecurityConfig struct {
	RootKeyFile            string   `json:"root_key_file"`
	SessionIdleTimeout     Duration `json:"session_idle_timeout"`
	SessionAbsoluteTimeout Duration `json:"session_absolute_timeout"`
}

type XrayConfig struct {
	APIEndpoint      string   `json:"api_endpoint"`
	SupportedVersion string   `json:"supported_version"`
	RPCTimeout       Duration `json:"rpc_timeout"`
}

type WorkersConfig struct {
	TrafficInterval   Duration `json:"traffic_interval"`
	ReconcileInterval Duration `json:"reconcile_interval"`
	MaxRetryInterval  Duration `json:"max_retry_interval"`
}

type InitialConfig struct {
	QuotaTimezone string `json:"quota_timezone"`
}

type LoggingConfig struct {
	Level string `json:"level"`
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	cfg := Default()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing config: %w", err)
	}
	return nil
}

func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen: "127.0.0.1:8080", ReadHeaderTimeout: Duration{5 * time.Second},
			RequestTimeout: Duration{15 * time.Second}, ShutdownTimeout: Duration{15 * time.Second},
		},
		Storage:  StorageConfig{BusyTimeout: Duration{5 * time.Second}},
		Security: SecurityConfig{SessionIdleTimeout: Duration{30 * time.Minute}, SessionAbsoluteTimeout: Duration{12 * time.Hour}},
		Xray:     XrayConfig{APIEndpoint: "127.0.0.1:10085", SupportedVersion: SupportedXrayVersion, RPCTimeout: Duration{5 * time.Second}},
		Workers:  WorkersConfig{TrafficInterval: Duration{5 * time.Second}, ReconcileInterval: Duration{15 * time.Second}, MaxRetryInterval: Duration{30 * time.Second}},
		Initial:  InitialConfig{QuotaTimezone: "UTC"},
		Logging:  LoggingConfig{Level: "info"},
	}
}

func (c *Config) Validate() error {
	c.Warnings = nil
	listenHost, err := socketHost(c.Server.Listen)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	xrayHost, err := socketHost(c.Xray.APIEndpoint)
	if err != nil {
		return fmt.Errorf("xray.api_endpoint: %w", err)
	}
	if !isLoopbackHost(xrayHost) {
		return errors.New("xray.api_endpoint must use a loopback address")
	}
	publicURL, err := url.Parse(c.Server.PublicURL)
	if err != nil || !publicURL.IsAbs() || publicURL.Host == "" || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return errors.New("server.public_url must be an absolute URL without user information, query, or fragment")
	}
	if c.Server.InsecureDevelopment {
		if !isLoopbackHost(listenHost) || !isLoopbackHost(xrayHost) {
			return errors.New("insecure_development requires loopback server and Xray endpoints")
		}
		c.Warnings = append(c.Warnings, "insecure development mode disables the Secure cookie requirement")
	} else if !strings.EqualFold(publicURL.Scheme, "https") {
		return errors.New("server.public_url must use HTTPS in production")
	}
	if err := positiveDurations(c); err != nil {
		return err
	}
	if c.Workers.TrafficInterval.Duration > 60*time.Second {
		return errors.New("workers.traffic_interval must not exceed 60s")
	}
	if c.Workers.TrafficInterval.Duration > 5*time.Second {
		c.Warnings = append(c.Warnings, "traffic interval exceeds 5s; quota overshoot risk is increased")
	}
	if c.Workers.ReconcileInterval.Duration > 30*time.Second {
		return errors.New("workers.reconcile_interval must not exceed 30s")
	}
	if c.Xray.SupportedVersion != SupportedXrayVersion {
		return fmt.Errorf("xray.supported_version must be %s", SupportedXrayVersion)
	}
	if _, err := time.LoadLocation(c.Initial.QuotaTimezone); err != nil {
		return errors.New("initial.quota_timezone must be a valid IANA timezone")
	}
	if c.Storage.DatabasePath == "" {
		return errors.New("storage.database_path is required")
	}
	if c.Security.RootKeyFile == "" {
		return errors.New("security.root_key_file is required")
	}
	dir := filepath.Dir(c.Storage.DatabasePath)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("storage.database_path parent directory must exist")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("storage database directory must not be group or world writable")
	}
	if err := validateLocalFilesystem(dir); err != nil {
		return err
	}
	return nil
}

func positiveDurations(c *Config) error {
	values := []struct {
		name  string
		value time.Duration
	}{
		{"server.read_header_timeout", c.Server.ReadHeaderTimeout.Duration},
		{"server.request_timeout", c.Server.RequestTimeout.Duration},
		{"server.shutdown_timeout", c.Server.ShutdownTimeout.Duration},
		{"storage.busy_timeout", c.Storage.BusyTimeout.Duration},
		{"security.session_idle_timeout", c.Security.SessionIdleTimeout.Duration},
		{"security.session_absolute_timeout", c.Security.SessionAbsoluteTimeout.Duration},
		{"xray.rpc_timeout", c.Xray.RPCTimeout.Duration},
		{"workers.traffic_interval", c.Workers.TrafficInterval.Duration},
		{"workers.reconcile_interval", c.Workers.ReconcileInterval.Duration},
		{"workers.max_retry_interval", c.Workers.MaxRetryInterval.Duration},
	}
	for _, item := range values {
		if item.value <= 0 {
			return fmt.Errorf("%s must be positive", item.name)
		}
	}
	if c.Security.SessionAbsoluteTimeout.Duration < c.Security.SessionIdleTimeout.Duration {
		return errors.New("session absolute timeout must not be shorter than idle timeout")
	}
	return nil
}

func socketHost(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return "", errors.New("must be one host:port socket address")
	}
	return host, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
