package apply

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cdn-console/edge-agent/internal/control"
)

type recordingRunner struct {
	calls      int
	failOnCall int
}

func (runner *recordingRunner) Run(_ context.Context, _ string, _ ...string) error {
	runner.calls++
	if runner.failOnCall == runner.calls {
		return errors.New("command failed")
	}
	return nil
}

type passingHealth struct{}

func (passingHealth) Check(_ context.Context, _ *url.URL) error { return nil }

type failingHealth struct{}

func (failingHealth) Check(_ context.Context, _ *url.URL) error {
	return errors.New("unhealthy")
}

func TestRollbackRestoresCurrentSymlinkWhenHealthFails(t *testing.T) {
	dataDirectory := t.TempDir()
	releases := filepath.Join(dataDirectory, "releases")
	first := filepath.Join(releases, "first")
	second := filepath.Join(releases, "second")
	for _, directory := range []string{first, second} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, filepath.Join(dataDirectory, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, filepath.Join(dataDirectory, "previous")); err != nil {
		t.Fatal(err)
	}
	healthURL, _ := url.Parse("http://127.0.0.1:8080/healthz")
	runner := &recordingRunner{}
	manager := &Manager{
		DataDir:       dataDirectory,
		ReloadCommand: []string{"openresty", "-s", "reload", "-p", "{current}"},
		HealthURL:     healthURL,
		Runner:        runner,
		Health:        failingHealth{},
	}

	result := manager.Rollback(context.Background())
	if result.Status != "failure" {
		t.Fatalf("Rollback() status = %s, want failure", result.Status)
	}
	current, err := os.Readlink(filepath.Join(dataDirectory, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current != first {
		t.Fatalf("current symlink = %s, want %s", current, first)
	}
	if runner.calls != 2 {
		t.Fatalf("reload calls = %d, want 2", runner.calls)
	}
}

func emptyReleaseManager(t *testing.T, runner *recordingRunner) *Manager {
	t.Helper()
	healthURL, err := url.Parse("http://127.0.0.1:8080/healthz")
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{
		DataDir:           t.TempDir(),
		ValidationCommand: []string{"openresty", "-t", "-p", "{staging}"},
		ReloadCommand:     []string{"openresty", "-s", "reload", "-p", "{current}"},
		HealthURL:         healthURL,
		Runner:            runner,
		Health:            passingHealth{},
		ReleaseRetention:  2,
	}
}

func TestApplyAuthoritativeEmptyRelease(t *testing.T) {
	runner := &recordingRunner{}
	manager := emptyReleaseManager(t, runner)
	result := manager.Apply(context.Background(), control.DesiredConfig{Generation: "empty-v1", Configs: []control.DesiredSiteConfig{}}, nil)
	if result.Status != "success" {
		t.Fatalf("Apply() = %#v, want success", result)
	}
	if runner.calls != 2 {
		t.Fatalf("commands = %d, want validation and reload", runner.calls)
	}
	current, err := os.Readlink(filepath.Join(manager.DataDir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(current, "conf", "nginx.conf"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := string(content)
	if info, err := os.Stat(filepath.Join(current, "conf", "stream", "services")); err != nil || !info.IsDir() {
		t.Fatalf("empty stream service directory unavailable: %v", err)
	}
	for _, want := range []string{"default_server", "location = /healthz", "location = /nginx_status", "return 404", "stream {", "include stream/services/*.conf"} {
		if !strings.Contains(configuration, want) {
			t.Fatalf("empty nginx config missing %q", want)
		}
	}
}

func TestApplyEmptyReleaseRollsBackWhenReloadFails(t *testing.T) {
	runner := &recordingRunner{failOnCall: 2}
	manager := emptyReleaseManager(t, runner)
	old := filepath.Join(manager.DataDir, "releases", "old")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, filepath.Join(manager.DataDir, "current")); err != nil {
		t.Fatal(err)
	}
	result := manager.Apply(context.Background(), control.DesiredConfig{Generation: "empty-v2", Configs: []control.DesiredSiteConfig{}}, nil)
	if result.Status != "rolled_back" {
		t.Fatalf("Apply() = %#v, want rolled_back", result)
	}
	current, err := os.Readlink(filepath.Join(manager.DataDir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current != old {
		t.Fatalf("current = %s, want %s", current, old)
	}
	if runner.calls != 3 {
		t.Fatalf("commands = %d, want validate, failed reload, restored reload", runner.calls)
	}
}
