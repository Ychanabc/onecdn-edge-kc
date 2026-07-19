package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cdn-console/edge-agent/internal/control"
)

const (
	testNodeID = "10000000-0000-4000-8000-000000000001"
	testSiteA  = "20000000-0000-4000-8000-00000000000a"
	testSiteB  = "20000000-0000-4000-8000-00000000000b"
	testTenant = "30000000-0000-4000-8000-000000000003"
)

func testCollector(t *testing.T, logPath string) *Collector {
	t.Helper()
	stateDir := t.TempDir()
	fixed := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	return NewCollector(Options{
		AccessLogPath: logPath,
		StateDir:      stateDir,
		NodeID:        testNodeID,
		Now:           func() time.Time { return fixed },
	})
}

func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
}

func appendLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
}

func accessLine(ts, siteID string, bytesSent int64) string {
	payload, err := json.Marshal(map[string]any{
		"timestamp":    ts,
		"tenant_id":    testTenant,
		"site_id":      siteID,
		"bytes_sent":   bytesSent,
		"request_time": 0.01,
		"status":       200,
	})
	if err != nil {
		panic(err)
	}
	return string(payload) + "\n"
}

func TestPrepareFirstBatchAggregatesAndSorts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	writeLog(t, logPath,
		accessLine("2026-07-13T01:00:00Z", testSiteB, 100),
		accessLine("2026-07-13T01:00:01Z", testSiteA, 50),
		accessLine("2026-07-13T01:00:02Z", testSiteA, 25),
	)
	collector := testCollector(t, logPath)

	report, err := collector.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if report == nil {
		t.Fatal("Prepare() report = nil")
	}
	if report.NodeID != testNodeID {
		t.Fatalf("NodeID = %q", report.NodeID)
	}
	if report.ReportID == "" {
		t.Fatal("ReportID empty")
	}
	if report.WindowStart.UTC().Format(time.RFC3339) != "2026-07-13T01:00:00Z" {
		t.Fatalf("WindowStart = %v", report.WindowStart)
	}
	if report.WindowEnd.UTC().Format(time.RFC3339) != "2026-07-13T01:00:02Z" {
		t.Fatalf("WindowEnd = %v", report.WindowEnd)
	}
	if len(report.Sites) != 2 {
		t.Fatalf("sites = %d, want 2", len(report.Sites))
	}
	if report.Sites[0].SiteID != testSiteA || report.Sites[0].EgressBytes != 75 || report.Sites[0].RequestCount != 2 {
		t.Fatalf("site[0] = %#v", report.Sites[0])
	}
	if report.Sites[1].SiteID != testSiteB || report.Sites[1].EgressBytes != 100 || report.Sites[1].RequestCount != 1 {
		t.Fatalf("site[1] = %#v", report.Sites[1])
	}
}

func TestPrepareResendsSamePendingUntilAck(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	writeLog(t, logPath, accessLine("2026-07-13T01:00:00Z", testSiteA, 10))
	collector := testCollector(t, logPath)

	first, err := collector.Prepare(context.Background())
	if err != nil || first == nil {
		t.Fatalf("first Prepare = %#v, err=%v", first, err)
	}
	appendLog(t, logPath, accessLine("2026-07-13T01:00:05Z", testSiteB, 99))

	second, err := collector.Prepare(context.Background())
	if err != nil || second == nil {
		t.Fatalf("second Prepare = %#v, err=%v", second, err)
	}
	if first.ReportID != second.ReportID {
		t.Fatalf("pending reportId changed: %s -> %s", first.ReportID, second.ReportID)
	}
	if len(second.Sites) != 1 || second.Sites[0].SiteID != testSiteA {
		t.Fatalf("pending mutated: %#v", second.Sites)
	}

	if err := collector.Ack(context.Background()); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	third, err := collector.Prepare(context.Background())
	if err != nil || third == nil {
		t.Fatalf("third Prepare = %#v, err=%v", third, err)
	}
	if third.ReportID == first.ReportID {
		t.Fatal("expected new reportId after ack")
	}
	if len(third.Sites) != 1 || third.Sites[0].SiteID != testSiteB || third.Sites[0].EgressBytes != 99 {
		t.Fatalf("post-ack batch = %#v", third.Sites)
	}
}

func TestPartialTrailingLineDeferred(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	partial := `{"timestamp":"2026-07-13T01:00:00Z","tenant_id":"` + testTenant +
		`","site_id":"` + testSiteA + `","bytes_sent":40,"request_time":0.1,"status":200`
	writeLog(t, logPath, partial)
	collector := testCollector(t, logPath)

	report, err := collector.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if report != nil {
		t.Fatalf("expected nil report for partial line, got %#v", report)
	}

	appendLog(t, logPath, "}\n")
	report, err = collector.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare(complete) error = %v", err)
	}
	if report == nil || len(report.Sites) != 1 || report.Sites[0].EgressBytes != 40 {
		t.Fatalf("completed partial = %#v", report)
	}
}

func TestMalformedCompleteLineSkipped(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	writeLog(t, logPath,
		"not-json\n",
		accessLine("2026-07-13T01:00:00Z", testSiteA, 7),
		`{"timestamp":"bad","site_id":"`+testSiteA+`","bytes_sent":1}`+"\n",
	)
	collector := testCollector(t, logPath)

	report, err := collector.Prepare(context.Background())
	if err != nil || report == nil {
		t.Fatalf("Prepare = %#v, err=%v", report, err)
	}
	if len(report.Sites) != 1 || report.Sites[0].EgressBytes != 7 || report.Sites[0].RequestCount != 1 {
		t.Fatalf("sites = %#v", report.Sites)
	}
	if err := collector.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, err := collector.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("expected no further data, got %#v", again)
	}
}

func TestRestartReadsPending(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	stateDir := filepath.Join(dir, "state")
	writeLog(t, logPath, accessLine("2026-07-13T01:00:00Z", testSiteA, 11))
	fixed := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	first := NewCollector(Options{
		AccessLogPath: logPath,
		StateDir:      stateDir,
		NodeID:        testNodeID,
		Now:           func() time.Time { return fixed },
	})
	report, err := first.Prepare(context.Background())
	if err != nil || report == nil {
		t.Fatalf("Prepare = %#v, err=%v", report, err)
	}

	restarted := NewCollector(Options{
		AccessLogPath: logPath,
		StateDir:      stateDir,
		NodeID:        testNodeID,
		Now:           func() time.Time { return fixed },
	})
	again, err := restarted.Prepare(context.Background())
	if err != nil || again == nil {
		t.Fatalf("restart Prepare = %#v, err=%v", again, err)
	}
	if again.ReportID != report.ReportID {
		t.Fatalf("reportId = %s, want %s", again.ReportID, report.ReportID)
	}
	raw, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	var decoded control.UsageReport
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ReportID != report.ReportID || len(decoded.Sites) != 1 {
		t.Fatalf("decoded pending = %#v", decoded)
	}
}

func TestTruncateResetsToZero(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	// Build a large committed offset, then shrink the file so size < offset.
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, accessLine("2026-07-13T01:00:00Z", testSiteA, int64(i+1)))
	}
	writeLog(t, logPath, lines...)
	collector := testCollector(t, logPath)
	report, err := collector.Prepare(context.Background())
	if err != nil || report == nil {
		t.Fatalf("Prepare = %#v, err=%v", report, err)
	}
	if err := collector.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := collector.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Committed.Offset == 0 {
		t.Fatal("expected non-zero committed offset before truncate")
	}

	writeLog(t, logPath, accessLine("2026-07-13T02:00:00Z", testSiteB, 22))
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= state.Committed.Offset {
		t.Fatalf("test setup: truncated size %d not below offset %d", info.Size(), state.Committed.Offset)
	}

	next, err := collector.Prepare(context.Background())
	if err != nil || next == nil {
		t.Fatalf("after truncate Prepare = %#v, err=%v", next, err)
	}
	if len(next.Sites) != 1 || next.Sites[0].SiteID != testSiteB || next.Sites[0].EgressBytes != 22 {
		t.Fatalf("truncate batch = %#v", next.Sites)
	}
}

func TestRenameRotationStartsNewFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	writeLog(t, logPath, accessLine("2026-07-13T01:00:00Z", testSiteA, 5))
	collector := testCollector(t, logPath)
	report, err := collector.Prepare(context.Background())
	if err != nil || report == nil {
		t.Fatalf("Prepare = %#v, err=%v", report, err)
	}
	if err := collector.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}

	rotated := filepath.Join(dir, "access.log.1")
	if err := os.Rename(logPath, rotated); err != nil {
		t.Fatal(err)
	}
	writeLog(t, logPath, accessLine("2026-07-13T03:00:00Z", testSiteB, 33))

	// Limitation under test: after rename the old inode is no longer at logPath;
	// unread tail of the old file (if any) is not recovered.
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	state, err := collector.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Committed.Inode == stat.Ino {
		t.Fatal("expected inode change after rename rotation")
	}

	next, err := collector.Prepare(context.Background())
	if err != nil || next == nil {
		t.Fatalf("rotation Prepare = %#v, err=%v", next, err)
	}
	if len(next.Sites) != 1 || next.Sites[0].SiteID != testSiteB {
		t.Fatalf("rotation batch = %#v", next.Sites)
	}
}

func TestPendingSurvivesRotationUntilAck(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	writeLog(t, logPath, accessLine("2026-07-13T01:00:00Z", testSiteA, 5))
	collector := testCollector(t, logPath)
	pending, err := collector.Prepare(context.Background())
	if err != nil || pending == nil {
		t.Fatalf("Prepare = %#v, err=%v", pending, err)
	}

	if err := os.Rename(logPath, filepath.Join(dir, "access.log.1")); err != nil {
		t.Fatal(err)
	}
	writeLog(t, logPath, accessLine("2026-07-13T03:00:00Z", testSiteB, 33))

	again, err := collector.Prepare(context.Background())
	if err != nil || again == nil {
		t.Fatalf("Prepare during rotation = %#v, err=%v", again, err)
	}
	if again.ReportID != pending.ReportID {
		t.Fatalf("pending replaced during rotation: %s -> %s", pending.ReportID, again.ReportID)
	}
	if err := collector.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := collector.Prepare(context.Background())
	if err != nil || next == nil {
		t.Fatalf("post-ack rotation Prepare = %#v, err=%v", next, err)
	}
	if next.Sites[0].SiteID != testSiteB {
		t.Fatalf("expected new-file site, got %#v", next.Sites)
	}
}
