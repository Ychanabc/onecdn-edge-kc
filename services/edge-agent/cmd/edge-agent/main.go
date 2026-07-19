package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cdn-console/edge-agent/internal/accessevents"
	"github.com/cdn-console/edge-agent/internal/agent"
	"github.com/cdn-console/edge-agent/internal/apply"
	"github.com/cdn-console/edge-agent/internal/config"
	"github.com/cdn-console/edge-agent/internal/control"
	"github.com/cdn-console/edge-agent/internal/identity"
	"github.com/cdn-console/edge-agent/internal/metrics"
	"github.com/cdn-console/edge-agent/internal/usage"
)

var version = "0.1.0"

func preflightDataDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(path, ".write-probe-*")
	if err != nil {
		return err
	}
	probePath := probe.Name()
	if err := probe.Chmod(0o600); err != nil {
		probe.Close()
		os.Remove(probePath)
		return err
	}
	if err := probe.Sync(); err != nil {
		probe.Close()
		os.Remove(probePath)
		return err
	}
	if err := probe.Close(); err != nil {
		os.Remove(probePath)
		return err
	}
	return os.Remove(probePath)
}

func run(ctx context.Context, logger *slog.Logger) error {
	configuration, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if err := preflightDataDirectory(configuration.DataDir); err != nil {
		return fmt.Errorf("data directory preflight: %w", err)
	}

	identityStore := identity.NewStore(configuration.DataDir)
	var nodeIdentity identity.Identity
	if identityStore.Exists() {
		nodeIdentity, err = identityStore.Load()
		if err != nil {
			return err
		}
	} else {
		bootstrapClient, err := control.NewBootstrapClient(
			configuration.ControlPlaneURL,
			configuration.ControlPlaneCA,
			configuration.ArtifactMaxBytes,
		)
		if err != nil {
			return err
		}
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("read hostname: %w", err)
		}
		nodeIdentity, err = identityStore.Bootstrap(
			ctx,
			bootstrapClient,
			configuration.BootstrapToken,
			version,
			hostname,
		)
		if err != nil {
			return fmt.Errorf("bootstrap edge identity: %w", err)
		}
		logger.Info("edge identity bootstrapped", "node_id", nodeIdentity.NodeID)
	}

	client, err := control.NewMTLSClient(
		configuration.ControlPlaneURL,
		identityStore.CertificatePath(),
		identityStore.KeyPath(),
		identityStore.CAPath(),
		configuration.ArtifactMaxBytes,
	)
	if err != nil {
		return err
	}
	signingKeys, err := identityStore.SigningKeys()
	if err != nil {
		return err
	}
	manager := &apply.Manager{
		DataDir:           configuration.DataDir,
		TenantID:          nodeIdentity.TenantID,
		AgentVersion:      version,
		ArtifactMaxBytes:  configuration.ArtifactMaxBytes,
		CommandTimeout:    configuration.CommandTimeout,
		ValidationCommand: configuration.ValidationCommand,
		ReloadCommand:     configuration.ReloadCommand,
		HealthURL:         configuration.HealthURL,
		ReleaseRetention:  configuration.ReleaseRetention,
		SigningKeys:       signingKeys,
		Runner:            apply.ExecRunner{},
		Health:            apply.NewHTTPHealthChecker(configuration.HealthTimeout),
	}
	logsDir := filepath.Join(configuration.DataDir, "logs")
	stateDir := filepath.Join(configuration.DataDir, "state")
	for _, directory := range []string{logsDir, stateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", directory, err)
		}
	}

	collector := metrics.NewCollector(metrics.Options{
		AccessLogPath: configuration.AccessLogFile,
		DataDir:       configuration.DataDir,
		HTTPTimeout:   configuration.HealthTimeout,
		StatusURL:     configuration.StatusURL,
	})

	var usageCollector agent.UsageCollector
	var accessEventCollector agent.AccessEventCollector
	if configuration.AccessLogFile != "" {
		if _, ok := any(client).(agent.UsageReporter); !ok {
			return errors.New("control plane client does not implement usage reporting")
		}
		accessEventCollector = accessevents.New(configuration.AccessLogFile, stateDir, nodeIdentity.NodeID)
		usageCollector = usage.NewCollector(usage.Options{
			AccessLogPath: configuration.AccessLogFile,
			StateDir:      stateDir,
			NodeID:        nodeIdentity.NodeID,
		})
	}

	daemon := &agent.Agent{
		Client:         client,
		Applier:        manager,
		Metrics:        collector,
		Usage:          usageCollector,
		AccessEvents:   accessEventCollector,
		StateStore:     agent.NewStateStore(configuration.DataDir),
		NodeID:         nodeIdentity.NodeID,
		Version:        version,
		DataDir:        configuration.DataDir,
		PollInterval:   configuration.PollInterval,
		MaxBackoff:     configuration.MaxBackoff,
		JitterFraction: configuration.JitterFraction,
		Logger:         logger,
	}
	logger.Info(
		"edge agent started",
		"data_dir", filepath.Clean(configuration.DataDir),
		"node_id", nodeIdentity.NodeID,
		"version", version,
	)
	return daemon.Run(ctx)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger); err != nil && ctx.Err() == nil {
		logger.Error("edge agent stopped", "error", err)
		os.Exit(1)
	}
}
