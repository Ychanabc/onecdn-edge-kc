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
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ControlPlaneURL   *url.URL
	ControlPlaneCA    string
	DataDir           string
	BootstrapToken    string
	CommandTimeout    time.Duration
	PollInterval      time.Duration
	MaxBackoff        time.Duration
	JitterFraction    float64
	ArtifactMaxBytes  int64
	ValidationCommand []string
	ReloadCommand     []string
	HealthURL         *url.URL
	HealthTimeout     time.Duration
	ReleaseRetention  int
	StatusURL         *url.URL
	AccessLogFile     string
}

type fileConfig struct {
	ControlPlaneURL   string   `json:"control_plane_url"`
	ControlPlaneCA    string   `json:"control_plane_ca_file"`
	DataDir           string   `json:"data_dir"`
	BootstrapToken    string   `json:"bootstrap_token"`
	CommandTimeout    string   `json:"command_timeout"`
	PollInterval      string   `json:"poll_interval"`
	MaxBackoff        string   `json:"max_backoff"`
	JitterFraction    *float64 `json:"jitter_fraction"`
	ArtifactMaxBytes  *int64   `json:"artifact_max_bytes"`
	ValidationCommand []string `json:"validation_command"`
	ReloadCommand     []string `json:"reload_command"`
	HealthURL         string   `json:"health_url"`
	HealthTimeout     string   `json:"health_timeout"`
	ReleaseRetention  *int     `json:"release_retention"`
	StatusURL         string   `json:"status_url"`
	AccessLogFile     string   `json:"access_log_file"`
}

func defaults() fileConfig {
	jitter := 0.2
	maxBytes := int64(64 * 1024 * 1024)
	retention := 5
	return fileConfig{
		ControlPlaneURL:   "https://127.0.0.1:3001",
		DataDir:           "/var/lib/cdn-edge-agent",
		CommandTimeout:    "30s",
		PollInterval:      "30s",
		MaxBackoff:        "5m",
		JitterFraction:    &jitter,
		ArtifactMaxBytes:  &maxBytes,
		ValidationCommand: []string{"openresty", "-t", "-p", "{staging}"},
		ReloadCommand:     []string{"openresty", "-s", "reload", "-p", "{current}"},
		HealthURL:         "http://127.0.0.1:8080/healthz",
		HealthTimeout:     "5s",
		ReleaseRetention:  &retention,
	}
}

func readFile(path string, target *fileConfig) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config file %s must not be accessible by group or others", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode config file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("config file contains trailing JSON values")
	}
	return nil
}

func overrideString(target *string, name string) {
	if value, ok := os.LookupEnv(name); ok {
		*target = value
	}
}

func overrideInt64(target **int64, name string) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be an integer: %w", name, err)
	}
	*target = &parsed
	return nil
}

func overrideInt(target **int, name string) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s must be an integer: %w", name, err)
	}
	*target = &parsed
	return nil
}

func overrideFloat(target **float64, name string) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s must be a number: %w", name, err)
	}
	*target = &parsed
	return nil
}

func overrideCommand(target *[]string, name string) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	var command []string
	if err := json.Unmarshal([]byte(value), &command); err != nil {
		return fmt.Errorf("%s must be a JSON array of arguments: %w", name, err)
	}
	*target = command
	return nil
}

func hasPlaceholder(command []string, placeholder string) bool {
	for _, argument := range command {
		if argument == placeholder {
			return true
		}
	}
	return false
}

func validateCommand(name string, command []string, placeholder string) error {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return fmt.Errorf("%s must contain an executable", name)
	}
	for _, argument := range command {
		if argument == "" || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("%s contains an empty or invalid argument", name)
		}
	}
	if !hasPlaceholder(command, placeholder) {
		return fmt.Errorf("%s must contain the exact %s argument", name, placeholder)
	}
	return nil
}

func parseDuration(name, value string, minimum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", name, err)
	}
	if duration < minimum {
		return 0, fmt.Errorf("%s must be at least %s", name, minimum)
	}
	return duration, nil
}

func Load() (Config, error) {
	raw := defaults()
	if path := os.Getenv("EDGE_AGENT_CONFIG_FILE"); path != "" {
		if err := readFile(path, &raw); err != nil {
			return Config{}, err
		}
	}

	overrideString(&raw.ControlPlaneURL, "EDGE_CONTROL_PLANE_URL")
	overrideString(&raw.ControlPlaneCA, "EDGE_CONTROL_PLANE_CA_FILE")
	overrideString(&raw.DataDir, "EDGE_DATA_DIR")
	overrideString(&raw.BootstrapToken, "EDGE_BOOTSTRAP_TOKEN")
	overrideString(&raw.CommandTimeout, "EDGE_COMMAND_TIMEOUT")
	overrideString(&raw.PollInterval, "EDGE_POLL_INTERVAL")
	overrideString(&raw.MaxBackoff, "EDGE_MAX_BACKOFF")
	overrideString(&raw.HealthURL, "EDGE_HEALTH_URL")
	overrideString(&raw.HealthTimeout, "EDGE_HEALTH_TIMEOUT")
	overrideString(&raw.StatusURL, "EDGE_STATUS_URL")
	overrideString(&raw.AccessLogFile, "EDGE_ACCESS_LOG_FILE")
	if err := overrideFloat(&raw.JitterFraction, "EDGE_JITTER_FRACTION"); err != nil {
		return Config{}, err
	}
	if err := overrideInt64(&raw.ArtifactMaxBytes, "EDGE_ARTIFACT_MAX_BYTES"); err != nil {
		return Config{}, err
	}
	if err := overrideInt(&raw.ReleaseRetention, "EDGE_RELEASE_RETENTION"); err != nil {
		return Config{}, err
	}
	if err := overrideCommand(&raw.ValidationCommand, "EDGE_VALIDATION_COMMAND"); err != nil {
		return Config{}, err
	}
	if err := overrideCommand(&raw.ReloadCommand, "EDGE_RELOAD_COMMAND"); err != nil {
		return Config{}, err
	}

	controlPlaneURL, err := url.Parse(raw.ControlPlaneURL)
	if err != nil || controlPlaneURL.Scheme != "https" || controlPlaneURL.Hostname() == "" {
		return Config{}, errors.New("control_plane_url must be an absolute https URL")
	}
	if controlPlaneURL.User != nil || controlPlaneURL.RawQuery != "" || controlPlaneURL.Fragment != "" {
		return Config{}, errors.New("control_plane_url must not contain credentials, query, or fragment")
	}
	if !filepath.IsAbs(raw.DataDir) {
		return Config{}, errors.New("data_dir must be an absolute path")
	}
	if raw.ControlPlaneCA != "" && !filepath.IsAbs(raw.ControlPlaneCA) {
		return Config{}, errors.New("control_plane_ca_file must be an absolute path")
	}

	healthURL, err := url.Parse(raw.HealthURL)
	if err != nil || healthURL.Scheme != "http" || healthURL.Hostname() == "" {
		return Config{}, errors.New("health_url must be an absolute local http URL")
	}
	healthHost := healthURL.Hostname()
	if healthHost != "localhost" && net.ParseIP(healthHost) == nil {
		return Config{}, errors.New("health_url must use localhost or an IP literal")
	}
	if ip := net.ParseIP(healthHost); ip != nil && !ip.IsLoopback() {
		return Config{}, errors.New("health_url IP must be loopback")
	}

	statusURL, err := resolveStatusURL(raw.StatusURL, healthURL)
	if err != nil {
		return Config{}, err
	}
	accessLogFile, err := resolveAccessLogFile(raw.AccessLogFile, filepath.Clean(raw.DataDir))
	if err != nil {
		return Config{}, err
	}

	pollInterval, err := parseDuration("poll_interval", raw.PollInterval, time.Second)
	if err != nil {
		return Config{}, err
	}
	maxBackoff, err := parseDuration("max_backoff", raw.MaxBackoff, pollInterval)
	if err != nil {
		return Config{}, err
	}
	healthTimeout, err := parseDuration("health_timeout", raw.HealthTimeout, 100*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	commandTimeout, err := parseDuration("command_timeout", raw.CommandTimeout, time.Second)
	if err != nil {
		return Config{}, err
	}
	if raw.JitterFraction == nil || *raw.JitterFraction < 0 || *raw.JitterFraction > 0.5 {
		return Config{}, errors.New("jitter_fraction must be between 0 and 0.5")
	}
	if raw.ArtifactMaxBytes == nil || *raw.ArtifactMaxBytes < 1024 || *raw.ArtifactMaxBytes > 1024*1024*1024 {
		return Config{}, errors.New("artifact_max_bytes must be between 1 KiB and 1 GiB")
	}
	if raw.ReleaseRetention == nil || *raw.ReleaseRetention < 2 || *raw.ReleaseRetention > 100 {
		return Config{}, errors.New("release_retention must be between 2 and 100")
	}
	if err := validateCommand("validation_command", raw.ValidationCommand, "{staging}"); err != nil {
		return Config{}, err
	}
	if err := validateCommand("reload_command", raw.ReloadCommand, "{current}"); err != nil {
		return Config{}, err
	}

	return Config{
		ControlPlaneURL:   controlPlaneURL,
		ControlPlaneCA:    raw.ControlPlaneCA,
		DataDir:           filepath.Clean(raw.DataDir),
		BootstrapToken:    strings.TrimSpace(raw.BootstrapToken),
		CommandTimeout:    commandTimeout,
		PollInterval:      pollInterval,
		MaxBackoff:        maxBackoff,
		JitterFraction:    *raw.JitterFraction,
		ArtifactMaxBytes:  *raw.ArtifactMaxBytes,
		ValidationCommand: append([]string(nil), raw.ValidationCommand...),
		ReloadCommand:     append([]string(nil), raw.ReloadCommand...),
		HealthURL:         healthURL,
		HealthTimeout:     healthTimeout,
		ReleaseRetention:  *raw.ReleaseRetention,
		StatusURL:         statusURL,
		AccessLogFile:     accessLogFile,
	}, nil
}

// resolveAccessLogFile locates the stable JSON access log used for error-rate
// metrics and durable usage collection. It defaults to DATA_DIR/logs/access.log
// (OpenResty edge_json). The literal "off" disables the reading.
func resolveAccessLogFile(raw, dataDir string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return filepath.Join(dataDir, "logs", "access.log"), nil
	}
	if strings.EqualFold(trimmed, "off") {
		return "", nil
	}
	if !filepath.IsAbs(trimmed) {
		return "", errors.New(`access_log_file must be an absolute path or "off"`)
	}
	return filepath.Clean(trimmed), nil
}

// resolveStatusURL returns the nginx stub_status endpoint. When unset it is
// derived from the health URL host and port so RPS works out of the box, since
// both are served by the local OpenResty instance over loopback.
func resolveStatusURL(raw string, health *url.URL) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return &url.URL{Scheme: "http", Host: health.Host, Path: "/nginx_status"}, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" {
		return nil, errors.New("status_url must be an absolute local http URL")
	}
	host := parsed.Hostname()
	if host != "localhost" && net.ParseIP(host) == nil {
		return nil, errors.New("status_url must use localhost or an IP literal")
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		return nil, errors.New("status_url IP must be loopback")
	}
	return parsed, nil
}
