package apply

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cdn-console/edge-agent/internal/artifact"
	"github.com/cdn-console/edge-agent/internal/control"
)

type Runner interface {
	Run(ctx context.Context, executable string, arguments ...string) error
}

type HealthChecker interface {
	Check(ctx context.Context, endpoint *url.URL) error
}

type ArtifactFetcher interface {
	DownloadArtifact(ctx context.Context, path string, expectedSize int64) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, arguments ...string) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", executable, err, strings.TrimSpace(string(output)))
	}
	return nil
}

type HTTPHealthChecker struct {
	client *http.Client
}

func NewHTTPHealthChecker(timeout time.Duration) *HTTPHealthChecker {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("health endpoint redirects are forbidden")
		},
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	return &HTTPHealthChecker{client: client}
}

func NewHTTPHealthCheckerWithClient(client *http.Client) *HTTPHealthChecker {
	return &HTTPHealthChecker{client: client}
}

func (checker *HTTPHealthChecker) Check(ctx context.Context, endpoint *url.URL) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	response, err := checker.client.Do(request)
	if err != nil {
		return fmt.Errorf("local health request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("local health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

type Manager struct {
	DataDir           string
	TenantID          string
	AgentVersion      string
	ArtifactMaxBytes  int64
	CommandTimeout    time.Duration
	ValidationCommand []string
	ReloadCommand     []string
	HealthURL         *url.URL
	ReleaseRetention  int
	SigningKeys       map[string]ed25519.PublicKey
	Runner            Runner
	Health            HealthChecker
}

type Result struct {
	Errors []string
	Status string
}

func commandArguments(command []string, placeholder, replacement string) (string, []string, error) {
	if len(command) == 0 {
		return "", nil, errors.New("command is empty")
	}
	arguments := make([]string, len(command)-1)
	found := false
	for index, argument := range command[1:] {
		if argument == placeholder {
			argument = replacement
			found = true
		}
		arguments[index] = argument
	}
	if command[0] == placeholder {
		return "", nil, errors.New("command executable cannot be a placeholder")
	}
	if !found {
		return "", nil, fmt.Errorf("command does not contain %s", placeholder)
	}
	return command[0], arguments, nil
}

func (manager *Manager) runCommand(ctx context.Context, command []string, placeholder, replacement string) error {
	executable, arguments, err := commandArguments(command, placeholder, replacement)
	if err != nil {
		return err
	}
	commandContext := ctx
	cancel := func() {}
	if manager.CommandTimeout > 0 {
		commandContext, cancel = context.WithTimeout(ctx, manager.CommandTimeout)
	}
	defer cancel()
	return manager.Runner.Run(commandContext, executable, arguments...)
}

func atomicSymlink(linkPath, target string) error {
	temporary := fmt.Sprintf("%s.tmp-%d", linkPath, os.Getpid())
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, linkPath); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func currentTarget(path string) (string, bool, error) {
	target, err := os.Readlink(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return target, true, nil
}

func generationDirectory(generation string) string {
	sum := sha256.Sum256([]byte(generation))
	return hex.EncodeToString(sum[:16])
}

func (manager *Manager) acquireLock() (*os.File, error) {
	if err := os.MkdirAll(manager.DataDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(manager.DataDir, "apply.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another configuration apply is already running")
	}
	return lock, nil
}

func releaseLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func rollbackCurrent(currentPath, oldTarget string, oldExists bool) error {
	if oldExists {
		return atomicSymlink(currentPath, oldTarget)
	}
	return os.Remove(currentPath)
}

func (manager *Manager) rollbackAfterActivation(ctx context.Context, currentPath, oldTarget string, oldExists bool, cause error) Result {
	errorsList := []string{cause.Error()}
	if err := rollbackCurrent(currentPath, oldTarget, oldExists); err != nil {
		errorsList = append(errorsList, "restore previous symlink: "+err.Error())
		return Result{Errors: errorsList, Status: "failure"}
	}
	if oldExists {
		if err := manager.runCommand(ctx, manager.ReloadCommand, "{current}", currentPath); err != nil {
			errorsList = append(errorsList, "reload restored release: "+err.Error())
		}
	}
	return Result{Errors: errorsList, Status: "rolled_back"}
}

const emptyReleaseNginxConfig = `worker_processes auto;
error_log stderr warn;
pid logs/nginx.pid;
events { worker_connections 4096; }
http {
  server_tokens off;
  server {
    listen 8080 default_server;
    server_name _;
    location = /healthz { access_log off; default_type text/plain; return 200 "ok\n"; }
    location = /nginx_status { allow 127.0.0.1; allow ::1; deny all; access_log off; stub_status; }
    location / { return 404; }
  }
}
stream {
  limit_conn_zone $binary_remote_addr zone=l4_per_ip:16m;
  include stream/services/*.conf;
}
`

func writeEmptyRelease(staging string) error {
	for _, directory := range []string{"conf/sites", "conf/modsecurity/custom", "conf/stream/services"} {
		if err := os.MkdirAll(filepath.Join(staging, directory), 0o750); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(staging, "conf/nginx.conf"), []byte(emptyReleaseNginxConfig), 0o640)
}

func (manager *Manager) Apply(ctx context.Context, desired control.DesiredConfig, fetcher ArtifactFetcher) Result {
	lock, err := manager.acquireLock()
	if err != nil {
		return Result{Errors: []string{err.Error()}, Status: "failure"}
	}
	defer releaseLock(lock)

	releasesPath := filepath.Join(manager.DataDir, "releases")
	if err := os.MkdirAll(releasesPath, 0o750); err != nil {
		return Result{Errors: []string{err.Error()}, Status: "failure"}
	}
	staging, err := os.MkdirTemp(releasesPath, ".staging-")
	if err != nil {
		return Result{Errors: []string{err.Error()}, Status: "failure"}
	}
	defer os.RemoveAll(staging)
	for _, directory := range []string{"cache", "logs"} {
		if err := os.MkdirAll(filepath.Join(staging, directory), 0o750); err != nil {
			return Result{Errors: []string{err.Error()}, Status: "failure"}
		}
	}
	if len(desired.Configs) == 0 {
		if err := writeEmptyRelease(staging); err != nil {
			return Result{Errors: []string{"stage empty release: " + err.Error()}, Status: "failure"}
		}
	}

	configs := append([]control.DesiredSiteConfig(nil), desired.Configs...)
	sort.Slice(configs, func(left, right int) bool {
		return configs[left].SiteID < configs[right].SiteID
	})
	for index, site := range configs {
		if index > 0 && configs[index-1].SiteID == site.SiteID {
			return Result{Errors: []string{"desired state contains duplicate site: " + site.SiteID}, Status: "failure"}
		}
		if site.Artifact.Manifest.TenantID != manager.TenantID ||
			site.Artifact.Manifest.SiteID != site.SiteID ||
			site.Artifact.Manifest.Generation != fmt.Sprintf("%s:%d", site.SiteID, site.Revision) {
			return Result{Errors: []string{"artifact manifest identity does not match desired state"}, Status: "failure"}
		}
		content, err := fetcher.DownloadArtifact(ctx, site.DownloadURL, site.Artifact.SizeBytes)
		if err != nil {
			return Result{Errors: []string{err.Error()}, Status: "failure"}
		}
		if err := artifact.VerifyAndExtract(
			content,
			site.Artifact,
			manager.SigningKeys,
			manager.ArtifactMaxBytes,
			manager.AgentVersion,
			staging,
		); err != nil {
			return Result{Errors: []string{err.Error()}, Status: "failure"}
		}
	}

	if err := manager.runCommand(ctx, manager.ValidationCommand, "{staging}", staging); err != nil {
		return Result{Errors: []string{"configuration validation failed: " + err.Error()}, Status: "failure"}
	}

	releasePath := filepath.Join(releasesPath, generationDirectory(desired.Generation))
	if _, err := os.Stat(releasePath); err == nil {
		releasePath = fmt.Sprintf("%s-%d", releasePath, time.Now().UnixNano())
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{Errors: []string{err.Error()}, Status: "failure"}
	}
	if err := os.Rename(staging, releasePath); err != nil {
		return Result{Errors: []string{"activate staged release: " + err.Error()}, Status: "failure"}
	}

	currentPath := filepath.Join(manager.DataDir, "current")
	previousPath := filepath.Join(manager.DataDir, "previous")
	oldTarget, oldExists, err := currentTarget(currentPath)
	if err != nil {
		return Result{Errors: []string{"read current release: " + err.Error()}, Status: "failure"}
	}
	priorPreviousTarget, priorPreviousExists, err := currentTarget(previousPath)
	if err != nil {
		return Result{Errors: []string{"read previous release: " + err.Error()}, Status: "failure"}
	}
	if oldExists {
		if err := atomicSymlink(previousPath, oldTarget); err != nil {
			return Result{Errors: []string{"record previous release: " + err.Error()}, Status: "failure"}
		}
	}
	if err := atomicSymlink(currentPath, releasePath); err != nil {
		errorsList := []string{"switch current release: " + err.Error()}
		if restoreErr := rollbackCurrent(previousPath, priorPreviousTarget, priorPreviousExists); restoreErr != nil {
			errorsList = append(errorsList, "restore previous pointer: "+restoreErr.Error())
		}
		return Result{Errors: errorsList, Status: "failure"}
	}
	rollbackApply := func(cause error) Result {
		result := manager.rollbackAfterActivation(ctx, currentPath, oldTarget, oldExists, cause)
		if err := rollbackCurrent(previousPath, priorPreviousTarget, priorPreviousExists); err != nil {
			result.Errors = append(result.Errors, "restore previous pointer: "+err.Error())
			result.Status = "failure"
		}
		return result
	}
	if err := manager.runCommand(ctx, manager.ReloadCommand, "{current}", currentPath); err != nil {
		return rollbackApply(err)
	}
	if err := manager.Health.Check(ctx, manager.HealthURL); err != nil {
		return rollbackApply(err)
	}
	manager.pruneReleases(currentPath, previousPath)
	return Result{Errors: []string{}, Status: "success"}
}

func (manager *Manager) Rollback(ctx context.Context) Result {
	lock, err := manager.acquireLock()
	if err != nil {
		return Result{Errors: []string{err.Error()}, Status: "failure"}
	}
	defer releaseLock(lock)

	currentPath := filepath.Join(manager.DataDir, "current")
	previousPath := filepath.Join(manager.DataDir, "previous")
	current, currentExists, err := currentTarget(currentPath)
	if err != nil || !currentExists {
		return Result{Errors: []string{"current release is unavailable"}, Status: "failure"}
	}
	previous, previousExists, err := currentTarget(previousPath)
	if err != nil || !previousExists {
		return Result{Errors: []string{"previous release is unavailable"}, Status: "failure"}
	}
	if err := atomicSymlink(currentPath, previous); err != nil {
		return Result{Errors: []string{err.Error()}, Status: "failure"}
	}
	if err := manager.runCommand(ctx, manager.ReloadCommand, "{current}", currentPath); err != nil {
		result := manager.rollbackAfterActivation(ctx, currentPath, current, true, err)
		result.Status = "failure"
		return result
	}
	if err := manager.Health.Check(ctx, manager.HealthURL); err != nil {
		result := manager.rollbackAfterActivation(ctx, currentPath, current, true, err)
		result.Status = "failure"
		return result
	}
	if err := atomicSymlink(previousPath, current); err != nil {
		result := manager.rollbackAfterActivation(ctx, currentPath, current, true, err)
		result.Status = "failure"
		return result
	}
	return Result{Errors: []string{}, Status: "rolled_back"}
}

func (manager *Manager) pruneReleases(currentPath, previousPath string) {
	entries, err := os.ReadDir(filepath.Join(manager.DataDir, "releases"))
	if err != nil {
		return
	}
	protected := make(map[string]struct{})
	for _, link := range []string{currentPath, previousPath} {
		if target, exists, _ := currentTarget(link); exists {
			protected[filepath.Clean(target)] = struct{}{}
		}
	}
	type release struct {
		modTime int64
		path    string
	}
	releases := make([]release, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".staging-") {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			releases = append(releases, release{
				modTime: info.ModTime().UnixNano(),
				path:    filepath.Join(manager.DataDir, "releases", entry.Name()),
			})
		}
	}
	sort.Slice(releases, func(left, right int) bool {
		return releases[left].modTime > releases[right].modTime
	})
	kept := 0
	for _, release := range releases {
		if _, isProtected := protected[filepath.Clean(release.path)]; isProtected {
			continue
		}
		kept++
		if kept > manager.ReleaseRetention {
			_ = os.RemoveAll(release.path)
		}
	}
}
