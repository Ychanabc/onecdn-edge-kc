package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cdn-console/edge-agent/internal/apply"
	"github.com/cdn-console/edge-agent/internal/control"
)

type unavailableControlPlane struct{}

func (unavailableControlPlane) Desired(context.Context, string) (*control.DesiredConfig, bool, error) {
	return nil, false, errors.New("control plane unavailable")
}

func (unavailableControlPlane) DownloadArtifact(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("unexpected download")
}

func (unavailableControlPlane) Report(context.Context, control.Report) error {
	return errors.New("control plane unavailable")
}

func (unavailableControlPlane) Ack(context.Context, control.Ack) error {
	return errors.New("unexpected ACK")
}

type forbiddenApplier struct {
	called bool
}

func (applier *forbiddenApplier) Apply(context.Context, control.DesiredConfig, apply.ArtifactFetcher) apply.Result {
	applier.called = true
	return apply.Result{Status: "failure"}
}

func (applier *forbiddenApplier) Rollback(context.Context) apply.Result {
	applier.called = true
	return apply.Result{Status: "failure"}
}

func TestControlPlaneOutageKeepsLastKnownGood(t *testing.T) {
	dataDirectory := t.TempDir()
	release := filepath.Join(dataDirectory, "releases", "known-good")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(dataDirectory, "current")
	if err := os.Symlink(release, currentPath); err != nil {
		t.Fatal(err)
	}
	stateStore := NewStateStore(dataDirectory)
	initial := State{
		AppliedGeneration: "sha256:known-good",
		ETag:              `"sha256:known-good"`,
		Status:            "healthy",
	}
	if err := stateStore.Save(initial); err != nil {
		t.Fatal(err)
	}
	applier := &forbiddenApplier{}
	daemon := &Agent{
		Client:       unavailableControlPlane{},
		Applier:      applier,
		StateStore:   stateStore,
		NodeID:       "10000000-0000-4000-8000-000000000001",
		Version:      "0.1.0",
		DataDir:      dataDirectory,
		PollInterval: time.Second,
		MaxBackoff:   time.Minute,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := daemon.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want control-plane outage")
	}
	if applier.called {
		t.Fatal("applier was called during control-plane outage")
	}
	current, err := os.Readlink(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if current != release {
		t.Fatalf("current symlink changed to %s, want %s", current, release)
	}
	state, err := stateStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != initial.AppliedGeneration || state.ETag != initial.ETag {
		t.Fatalf("state changed during outage: %#v", state)
	}
}

type emptyReleaseControlPlane struct {
	desired control.DesiredConfig
	acks    []control.Ack
	reports []control.Report
}

func (plane *emptyReleaseControlPlane) Desired(context.Context, string) (*control.DesiredConfig, bool, error) {
	return &plane.desired, false, nil
}
func (*emptyReleaseControlPlane) DownloadArtifact(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("empty release must not download artifacts")
}
func (plane *emptyReleaseControlPlane) Report(_ context.Context, report control.Report) error {
	plane.reports = append(plane.reports, report)
	return nil
}
func (plane *emptyReleaseControlPlane) Ack(_ context.Context, ack control.Ack) error {
	plane.acks = append(plane.acks, ack)
	return nil
}

type successfulEmptyApplier struct{ applied []control.DesiredConfig }

func (applier *successfulEmptyApplier) Apply(_ context.Context, desired control.DesiredConfig, _ apply.ArtifactFetcher) apply.Result {
	applier.applied = append(applier.applied, desired)
	return apply.Result{Errors: []string{}, Status: "success"}
}
func (*successfulEmptyApplier) Rollback(context.Context) apply.Result {
	return apply.Result{Status: "failure"}
}

func TestAuthoritativeEmptyReleaseIsAppliedAndAcknowledged(t *testing.T) {
	dataDirectory := t.TempDir()
	plane := &emptyReleaseControlPlane{desired: control.DesiredConfig{
		Certificates: []control.DesiredCertificate{},
		Configs:      []control.DesiredSiteConfig{},
		ETag:         `"empty-v1"`, Generation: "empty-v1",
		NodeID: "10000000-0000-4000-8000-000000000001",
	}}
	applier := &successfulEmptyApplier{}
	daemon := &Agent{
		Client: plane, Applier: applier, StateStore: NewStateStore(dataDirectory),
		NodeID: plane.desired.NodeID, Version: "0.1.0",
	}
	if err := daemon.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(applier.applied) != 1 || len(applier.applied[0].Configs) != 0 {
		t.Fatalf("empty release was not applied: %#v", applier.applied)
	}
	if len(plane.acks) != 1 || plane.acks[0].Status != "success" || plane.acks[0].Generation != "empty-v1" {
		t.Fatalf("ACKs = %#v, want successful empty-v1 ACK", plane.acks)
	}
	state, err := daemon.StateStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.AppliedGeneration != "empty-v1" || state.ETag != `"empty-v1"` || state.Status != "healthy" {
		t.Fatalf("state = %#v", state)
	}
}
