package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cdn-console/edge-agent/internal/control"
	"github.com/cdn-console/edge-agent/internal/usage"
)

type usageRecordingPlane struct {
	recordingControlPlane
	usageErr error
	usages   []control.UsageReport
}

func (plane *usageRecordingPlane) Usage(_ context.Context, report control.UsageReport) error {
	plane.usages = append(plane.usages, report)
	return plane.usageErr
}

func TestRunOnceUploadsUsageAfterReconcile(t *testing.T) {
	dataDirectory := t.TempDir()
	stateStore := NewStateStore(dataDirectory)
	if err := stateStore.Save(State{
		AppliedGeneration: "gen-1",
		ETag:              `"etag-1"`,
		Status:            "healthy",
	}); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dataDirectory, "logs", "access.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-07-13T01:00:00Z","tenant_id":"30000000-0000-4000-8000-000000000003","site_id":"20000000-0000-4000-8000-00000000000a","bytes_sent":12,"request_time":0.01,"status":200}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := usage.NewCollector(usage.Options{
		AccessLogPath: logPath,
		StateDir:      filepath.Join(dataDirectory, "state"),
		NodeID:        "10000000-0000-4000-8000-000000000001",
		Now:           func() time.Time { return time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC) },
	})

	plane := &usageRecordingPlane{recordingControlPlane: recordingControlPlane{notModified: true}}
	daemon := &Agent{
		Client:       plane,
		Applier:      &forbiddenApplier{},
		Usage:        collector,
		StateStore:   stateStore,
		NodeID:       "10000000-0000-4000-8000-000000000001",
		Version:      "0.1.0",
		DataDir:      dataDirectory,
		PollInterval: time.Second,
		MaxBackoff:   time.Minute,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if err := daemon.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(plane.usages) != 1 {
		t.Fatalf("usages = %d, want 1", len(plane.usages))
	}
	if plane.usages[0].Sites[0].EgressBytes != 12 {
		t.Fatalf("usage = %#v", plane.usages[0])
	}
}

func TestRunOnceKeepsPendingWhenUsageFails(t *testing.T) {
	dataDirectory := t.TempDir()
	stateStore := NewStateStore(dataDirectory)
	if err := stateStore.Save(State{
		AppliedGeneration: "gen-1",
		ETag:              `"etag-1"`,
		Status:            "healthy",
	}); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dataDirectory, "logs", "access.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-07-13T01:00:00Z","tenant_id":"30000000-0000-4000-8000-000000000003","site_id":"20000000-0000-4000-8000-00000000000a","bytes_sent":12,"request_time":0.01,"status":200}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := usage.NewCollector(usage.Options{
		AccessLogPath: logPath,
		StateDir:      filepath.Join(dataDirectory, "state"),
		NodeID:        "10000000-0000-4000-8000-000000000001",
	})

	plane := &usageRecordingPlane{
		recordingControlPlane: recordingControlPlane{notModified: true},
		usageErr:              errors.New("5xx"),
	}
	daemon := &Agent{
		Client:       plane,
		Applier:      &forbiddenApplier{},
		Usage:        collector,
		StateStore:   stateStore,
		NodeID:       "10000000-0000-4000-8000-000000000001",
		Version:      "0.1.0",
		DataDir:      dataDirectory,
		PollInterval: time.Second,
		MaxBackoff:   time.Minute,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if err := daemon.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want usage failure")
	}
	if len(plane.usages) != 1 {
		t.Fatalf("usages = %d, want 1", len(plane.usages))
	}
	reportID := plane.usages[0].ReportID

	plane.usageErr = nil
	if err := daemon.RunOnce(context.Background()); err != nil {
		t.Fatalf("retry RunOnce() error = %v", err)
	}
	if len(plane.usages) != 2 {
		t.Fatalf("usages after retry = %d, want 2", len(plane.usages))
	}
	if plane.usages[1].ReportID != reportID {
		t.Fatalf("reportId changed on retry: %s -> %s", reportID, plane.usages[1].ReportID)
	}
}
