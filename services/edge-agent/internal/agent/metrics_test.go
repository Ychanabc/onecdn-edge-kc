package agent

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cdn-console/edge-agent/internal/control"
)

type recordingControlPlane struct {
	notModified bool
	reports     []control.Report
}

func (plane *recordingControlPlane) Desired(context.Context, string) (*control.DesiredConfig, bool, error) {
	return nil, plane.notModified, nil
}

func (recordingControlPlane) DownloadArtifact(context.Context, string, int64) ([]byte, error) {
	return nil, nil
}

func (plane *recordingControlPlane) Report(_ context.Context, report control.Report) error {
	plane.reports = append(plane.reports, report)
	return nil
}

func (recordingControlPlane) Ack(context.Context, control.Ack) error { return nil }

type staticMetrics struct {
	value control.NodeMetrics
}

func (collector staticMetrics) Collect(context.Context) control.NodeMetrics {
	return collector.value
}

func TestReportUsesMetricsCollector(t *testing.T) {
	dataDirectory := t.TempDir()
	stateStore := NewStateStore(dataDirectory)
	if err := stateStore.Save(State{
		AppliedGeneration: "gen-1",
		ETag:              `"etag-1"`,
		Status:            "healthy",
	}); err != nil {
		t.Fatal(err)
	}

	wanted := control.NodeMetrics{
		BandwidthInMbps:   12.5,
		BandwidthOutMbps:  8.25,
		CollectedAt:       time.Unix(1_700_000_000, 0).UTC(),
		CPUPercent:        42,
		DiskPercent:       33,
		ErrorRatePercent:  1.5,
		MemoryPercent:     55,
		RequestsPerSecond: 7,
	}
	plane := &recordingControlPlane{notModified: true}
	daemon := &Agent{
		Client:       plane,
		Applier:      &forbiddenApplier{},
		Metrics:      staticMetrics{value: wanted},
		StateStore:   stateStore,
		NodeID:       "10000000-0000-4000-8000-000000000001",
		Version:      "9.9.9",
		DataDir:      dataDirectory,
		PollInterval: time.Second,
		MaxBackoff:   time.Minute,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := daemon.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(plane.reports))
	}
	if plane.reports[0].Metrics != wanted {
		t.Fatalf("reported metrics = %#v, want %#v", plane.reports[0].Metrics, wanted)
	}
}

func TestCollectMetricsNilCollector(t *testing.T) {
	daemon := &Agent{}
	snapshot := daemon.collectMetrics(context.Background())
	if snapshot.CollectedAt.IsZero() {
		t.Fatal("CollectedAt was not set for nil collector")
	}
	if snapshot.CPUPercent != 0 || snapshot.RequestsPerSecond != 0 {
		t.Fatalf("expected zeroed metrics for nil collector, got %#v", snapshot)
	}
}
