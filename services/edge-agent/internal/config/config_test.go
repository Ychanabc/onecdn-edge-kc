package config

import (
	"path/filepath"
	"testing"
)

func loadWithoutFile(t *testing.T) Config {
	t.Helper()
	t.Setenv("EDGE_AGENT_CONFIG_FILE", "")
	configuration, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return configuration
}

func TestLoadDerivesTelemetryDefaults(t *testing.T) {
	configuration := loadWithoutFile(t)

	if configuration.StatusURL == nil ||
		configuration.StatusURL.String() != "http://127.0.0.1:8080/nginx_status" {
		t.Fatalf("StatusURL = %v, want derived /nginx_status", configuration.StatusURL)
	}
	wantLog := filepath.Join(configuration.DataDir, "logs", "access.log")
	if configuration.AccessLogFile != wantLog {
		t.Fatalf("AccessLogFile = %q, want %q", configuration.AccessLogFile, wantLog)
	}
}

func TestLoadStatusURLOverride(t *testing.T) {
	t.Setenv("EDGE_STATUS_URL", "http://127.0.0.1:9999/status")
	configuration := loadWithoutFile(t)
	if configuration.StatusURL.String() != "http://127.0.0.1:9999/status" {
		t.Fatalf("StatusURL = %v, want override", configuration.StatusURL)
	}
}

func TestLoadStatusURLRejectsNonLoopback(t *testing.T) {
	t.Setenv("EDGE_AGENT_CONFIG_FILE", "")
	t.Setenv("EDGE_STATUS_URL", "http://10.0.0.1/nginx_status")
	if _, err := Load(); err == nil {
		t.Fatal("expected non-loopback status URL to be rejected")
	}
}

func TestLoadAccessLogOffDisables(t *testing.T) {
	t.Setenv("EDGE_ACCESS_LOG_FILE", "off")
	configuration := loadWithoutFile(t)
	if configuration.AccessLogFile != "" {
		t.Fatalf("AccessLogFile = %q, want empty when disabled", configuration.AccessLogFile)
	}
}

func TestLoadAccessLogCustomPath(t *testing.T) {
	t.Setenv("EDGE_ACCESS_LOG_FILE", "/var/log/custom/access.log")
	configuration := loadWithoutFile(t)
	if configuration.AccessLogFile != "/var/log/custom/access.log" {
		t.Fatalf("AccessLogFile = %q, want custom path", configuration.AccessLogFile)
	}
}

func TestLoadAccessLogRejectsRelative(t *testing.T) {
	t.Setenv("EDGE_AGENT_CONFIG_FILE", "")
	t.Setenv("EDGE_ACCESS_LOG_FILE", "relative/access.log")
	if _, err := Load(); err == nil {
		t.Fatal("expected relative access log path to be rejected")
	}
}
