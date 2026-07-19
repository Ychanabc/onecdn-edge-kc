package agent

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cdn-console/edge-agent/internal/apply"
	"github.com/cdn-console/edge-agent/internal/control"
)

type ControlPlane interface {
	Desired(ctx context.Context, etag string) (*control.DesiredConfig, bool, error)
	DownloadArtifact(ctx context.Context, path string, expectedSize int64) ([]byte, error)
	Report(ctx context.Context, report control.Report) error
	Ack(ctx context.Context, ack control.Ack) error
}

// UsageReporter uploads durable egress usage. Production clients implement this;
// test fakes may omit it when Usage is nil.
type UsageReporter interface {
	Usage(ctx context.Context, report control.UsageReport) error
}

type AccessEventReporter interface {
	AccessEvents(ctx context.Context, batch control.AccessEventBatch) error
}
type AccessEventCollector interface {
	Prepare(ctx context.Context) (*control.AccessEventBatch, error)
	Ack(ctx context.Context) error
}

type Applier interface {
	Apply(ctx context.Context, desired control.DesiredConfig, fetcher apply.ArtifactFetcher) apply.Result
	Rollback(ctx context.Context) apply.Result
}

// MetricsCollector samples node telemetry for each report. A nil collector
// causes the agent to report an empty metrics snapshot.
type MetricsCollector interface {
	Collect(ctx context.Context) control.NodeMetrics
}

// UsageCollector prepares and acknowledges durable usage reports. A nil
// collector skips usage upload (tests / access-log disabled).
type UsageCollector interface {
	Prepare(ctx context.Context) (*control.UsageReport, error)
	Ack(ctx context.Context) error
}

type Agent struct {
	Client         ControlPlane
	Applier        Applier
	Metrics        MetricsCollector
	Usage          UsageCollector
	AccessEvents   AccessEventCollector
	StateStore     *StateStore
	NodeID         string
	Version        string
	DataDir        string
	PollInterval   time.Duration
	MaxBackoff     time.Duration
	JitterFraction float64
	Logger         *slog.Logger
}

func limitedErrors(errorsList []string) []string {
	result := make([]string, 0, len(errorsList))
	for _, message := range errorsList {
		if len(message) > 2000 {
			message = message[:2000]
		}
		result = append(result, message)
	}
	return result
}

func (agent *Agent) collectMetrics(ctx context.Context) control.NodeMetrics {
	if agent.Metrics == nil {
		return control.NodeMetrics{CollectedAt: time.Now().UTC()}
	}
	return agent.Metrics.Collect(ctx)
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (agent *Agent) report(ctx context.Context, state State, errorsList []string) error {
	return agent.Client.Report(ctx, control.Report{
		AppliedGeneration: stringPointer(state.AppliedGeneration),
		Errors:            limitedErrors(errorsList),
		Metrics:           agent.collectMetrics(ctx),
		NodeID:            agent.NodeID,
		ReportedAt:        time.Now().UTC(),
		Status:            state.Status,
		Version:           agent.Version,
	})
}

func (agent *Agent) sendPendingAck(ctx context.Context, state *State) error {
	if state.PendingAck == nil {
		return nil
	}
	if err := agent.Client.Ack(ctx, *state.PendingAck); err != nil {
		return err
	}
	state.PendingAck = nil
	return agent.StateStore.Save(*state)
}

func (agent *Agent) queueAck(state *State, desired control.DesiredConfig, commandID *string, result apply.Result) error {
	state.PendingAck = &control.Ack{
		AppliedAt:  time.Now().UTC(),
		CommandID:  commandID,
		Errors:     limitedErrors(result.Errors),
		Generation: desired.Generation,
		NodeID:     agent.NodeID,
		Status:     result.Status,
	}
	return agent.StateStore.Save(*state)
}

func (agent *Agent) handleCommand(ctx context.Context, state *State, desired control.DesiredConfig) (bool, []string, error) {
	command := desired.Command
	if command == nil || command.ID == state.LastCommandID {
		return false, nil, nil
	}

	result := apply.Result{Errors: []string{}, Status: "success"}
	switch command.Type {
	case "drain":
		state.Status = "draining"
	case "maintenance":
		state.Status = "maintenance"
	case "rollback":
		result = agent.Applier.Rollback(ctx)
		if result.Status == "rolled_back" {
			state.AppliedGeneration, state.PreviousGeneration =
				state.PreviousGeneration, state.AppliedGeneration
			state.Status = "healthy"
		}
	default:
		result = apply.Result{
			Errors: []string{"unsupported edge command: " + command.Type},
			Status: "failure",
		}
	}
	if result.Status != "failure" {
		state.LastCommandID = command.ID
		state.ETag = desired.ETag
	}
	if err := agent.queueAck(state, desired, &command.ID, result); err != nil {
		return true, result.Errors, err
	}
	if err := agent.sendPendingAck(ctx, state); err != nil {
		return true, result.Errors, err
	}
	return true, result.Errors, nil
}

func (agent *Agent) reportUsage(ctx context.Context) error {
	if agent.Usage == nil {
		return nil
	}
	report, err := agent.Usage.Prepare(ctx)
	if err != nil {
		return fmt.Errorf("prepare usage report: %w", err)
	}
	if report == nil {
		return nil
	}
	reporter, ok := agent.Client.(UsageReporter)
	if !ok {
		return errors.New("control plane client does not support usage reporting")
	}
	if err := reporter.Usage(ctx, *report); err != nil {
		return fmt.Errorf("upload usage report: %w", err)
	}
	if err := agent.Usage.Ack(ctx); err != nil {
		return fmt.Errorf("ack usage report: %w", err)
	}
	return nil
}

func (agent *Agent) RunOnce(ctx context.Context) error {
	if err := agent.reconcileOnce(ctx); err != nil {
		return err
	}
	if err := agent.reportUsage(ctx); err != nil {
		return err
	}
	if agent.AccessEvents == nil {
		return nil
	}
	batch, err := agent.AccessEvents.Prepare(ctx)
	if err != nil {
		return fmt.Errorf("prepare access events: %w", err)
	}
	if batch == nil {
		return nil
	}
	reporter, ok := agent.Client.(AccessEventReporter)
	if !ok {
		return errors.New("control plane client does not support access events")
	}
	if err := reporter.AccessEvents(ctx, *batch); err != nil {
		return fmt.Errorf("upload access events: %w", err)
	}
	return agent.AccessEvents.Ack(ctx)
}

func (agent *Agent) reconcileOnce(ctx context.Context) error {
	state, err := agent.StateStore.Load()
	if err != nil {
		return err
	}
	if err := agent.sendPendingAck(ctx, &state); err != nil {
		return fmt.Errorf("resend pending ACK: %w", err)
	}

	desired, notModified, err := agent.Client.Desired(ctx, state.ETag)
	if err != nil {
		_ = agent.report(ctx, state, []string{err.Error()})
		return err
	}
	if notModified {
		return agent.report(ctx, state, nil)
	}
	if desired == nil {
		return errors.New("control plane returned empty desired state")
	}
	if desired.NodeID != agent.NodeID {
		return errors.New("desired state node identity mismatch")
	}

	handled, commandErrors, err := agent.handleCommand(ctx, &state, *desired)
	if err != nil {
		_ = agent.report(ctx, state, append(commandErrors, err.Error()))
		return err
	}
	if handled {
		return agent.report(ctx, state, commandErrors)
	}
	if state.Status == "draining" || state.Status == "maintenance" {
		state.ETag = desired.ETag
		if err := agent.StateStore.Save(state); err != nil {
			return err
		}
		return agent.report(ctx, state, nil)
	}
	if desired.Generation == state.AppliedGeneration {
		state.ETag = desired.ETag
		if err := agent.StateStore.Save(state); err != nil {
			return err
		}
		return agent.report(ctx, state, nil)
	}

	result := agent.Applier.Apply(ctx, *desired, agent.Client)
	if result.Status == "success" {
		state.PreviousGeneration = state.AppliedGeneration
		state.AppliedGeneration = desired.Generation
		state.ETag = desired.ETag
		state.Status = "healthy"
	} else {
		state.Status = "warning"
	}
	if err := agent.queueAck(&state, *desired, nil, result); err != nil {
		return err
	}
	if err := agent.sendPendingAck(ctx, &state); err != nil {
		_ = agent.report(ctx, state, append(result.Errors, err.Error()))
		return err
	}
	if err := agent.report(ctx, state, result.Errors); err != nil {
		return err
	}
	if result.Status != "success" {
		return fmt.Errorf("desired state apply failed: %v", result.Errors)
	}
	return nil
}

func jitter(duration time.Duration, fraction float64) time.Duration {
	if fraction <= 0 {
		return duration
	}
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return duration
	}
	unit := float64(binary.BigEndian.Uint64(randomBytes[:])) / float64(^uint64(0))
	factor := 1 - fraction + (2 * fraction * unit)
	return time.Duration(float64(duration) * factor)
}

func (agent *Agent) Run(ctx context.Context) error {
	delay := time.Duration(0)
	backoff := agent.PollInterval
	for {
		if delay > 0 {
			timer := time.NewTimer(jitter(delay, agent.JitterFraction))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		err := agent.RunOnce(ctx)
		if err != nil {
			agent.Logger.Error("edge reconciliation failed; retaining last-known-good release", "error", err)
			delay = backoff
			backoff *= 2
			if backoff > agent.MaxBackoff {
				backoff = agent.MaxBackoff
			}
		} else {
			delay = agent.PollInterval
			backoff = agent.PollInterval
		}
	}
}
