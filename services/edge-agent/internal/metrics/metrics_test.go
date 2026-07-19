package metrics

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func netDevContent(loRx, loTx, ethRx, ethTx int64) string {
	header := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"
	lo := fmt.Sprintf("    lo: %d 1 0 0 0 0 0 0 %d 1 0 0 0 0 0 0\n", loRx, loTx)
	eth := fmt.Sprintf("  eth0: %d 10 0 0 0 0 0 0 %d 12 0 0 0 0 0 0\n", ethRx, ethTx)
	return header + lo + eth
}

func approxEqual(actual, expected float64) bool {
	return math.Abs(actual-expected) <= 1e-6
}

func TestCollectProcMetrics(t *testing.T) {
	procRoot := t.TempDir()
	statPath := filepath.Join(procRoot, "stat")
	meminfoPath := filepath.Join(procRoot, "meminfo")
	netPath := filepath.Join(procRoot, "net", "dev")

	writeFile(t, statPath, "cpu 0 0 0 0 0 0 0 0\ncpu0 0 0 0 0\n")
	writeFile(t, meminfoPath, "MemTotal:       1000 kB\nMemAvailable:    250 kB\nMemFree:  100 kB\n")
	writeFile(t, netPath, netDevContent(1_000_000_000, 1_000_000_000, 5_000_000, 6_000_000))

	current := time.Unix(1_700_000_000, 0).UTC()
	collector := NewCollector(Options{
		DataDir:  t.TempDir(),
		Now:      func() time.Time { return current },
		ProcRoot: procRoot,
	})

	first := collector.Collect(context.Background())
	if first.CPUPercent != 0 {
		t.Fatalf("first CPU sample = %v, want 0 baseline", first.CPUPercent)
	}
	if !approxEqual(first.MemoryPercent, 75) {
		t.Fatalf("memory = %v, want 75", first.MemoryPercent)
	}
	if first.BandwidthInMbps != 0 || first.BandwidthOutMbps != 0 {
		t.Fatalf("first bandwidth = %v/%v, want 0 baseline", first.BandwidthInMbps, first.BandwidthOutMbps)
	}

	// CPU: total +1000, idle +600 => 40% busy.
	writeFile(t, statPath, "cpu 400 0 0 600 0 0 0 0\ncpu0 0 0 0 0\n")
	// Net over 8s: eth0 rx +1_000_000 bytes, tx +2_000_000 bytes. lo changes by 1e9
	// and must be excluded.
	writeFile(t, netPath, netDevContent(2_000_000_000, 2_000_000_000, 6_000_000, 8_000_000))
	current = current.Add(8 * time.Second)

	second := collector.Collect(context.Background())
	if !approxEqual(second.CPUPercent, 40) {
		t.Fatalf("CPU = %v, want 40", second.CPUPercent)
	}
	if !approxEqual(second.BandwidthInMbps, 1) {
		t.Fatalf("bandwidth in = %v, want 1", second.BandwidthInMbps)
	}
	if !approxEqual(second.BandwidthOutMbps, 2) {
		t.Fatalf("bandwidth out = %v, want 2", second.BandwidthOutMbps)
	}
}

func TestCollectRequestsPerSecond(t *testing.T) {
	requests := 100
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(writer,
			"Active connections: 3 \nserver accepts handled requests\n %d %d %d \nReading: 0 Writing: 1 Waiting: 2 \n",
			0, 0, requests)
	}))
	defer server.Close()
	statusURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	current := time.Unix(1_700_000_000, 0).UTC()
	collector := NewCollector(Options{
		DataDir:    t.TempDir(),
		HTTPClient: server.Client(),
		Now:        func() time.Time { return current },
		ProcRoot:   t.TempDir(),
		StatusURL:  statusURL,
	})

	if first := collector.Collect(context.Background()); first.RequestsPerSecond != 0 {
		t.Fatalf("first RPS = %v, want 0 baseline", first.RequestsPerSecond)
	}

	requests = 400
	current = current.Add(30 * time.Second)
	second := collector.Collect(context.Background())
	if !approxEqual(second.RequestsPerSecond, 10) {
		t.Fatalf("RPS = %v, want 10", second.RequestsPerSecond)
	}
}

func appendAccessLog(t *testing.T, path string, statuses ...int) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, status := range statuses {
		if _, err := fmt.Fprintf(file, "{\"status\":%d,\"host\":\"example.test\"}\n", status); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectErrorRate(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.json")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	current := time.Unix(1_700_000_000, 0).UTC()
	collector := NewCollector(Options{
		AccessLogPath: logPath,
		DataDir:       t.TempDir(),
		Now:           func() time.Time { return current },
		ProcRoot:      t.TempDir(),
	})

	if baseline := collector.Collect(context.Background()); baseline.ErrorRatePercent != 0 {
		t.Fatalf("baseline error rate = %v, want 0", baseline.ErrorRatePercent)
	}

	appendAccessLog(t, logPath, 200, 200, 200, 200, 200, 200, 200, 200, 500, 502)
	current = current.Add(30 * time.Second)
	windowed := collector.Collect(context.Background())
	if !approxEqual(windowed.ErrorRatePercent, 20) {
		t.Fatalf("error rate = %v, want 20", windowed.ErrorRatePercent)
	}

	// A subsequent window with no new failures reports 0.
	appendAccessLog(t, logPath, 200, 200)
	current = current.Add(30 * time.Second)
	if healthy := collector.Collect(context.Background()); healthy.ErrorRatePercent != 0 {
		t.Fatalf("healthy window error rate = %v, want 0", healthy.ErrorRatePercent)
	}
}

func TestErrorRateHandlesTruncation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.json")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	current := time.Unix(1_700_000_000, 0).UTC()
	collector := NewCollector(Options{
		AccessLogPath: logPath,
		DataDir:       t.TempDir(),
		Now:           func() time.Time { return current },
		ProcRoot:      t.TempDir(),
	})

	collector.Collect(context.Background())
	appendAccessLog(t, logPath, 200, 500, 500, 500)
	current = current.Add(30 * time.Second)
	collector.Collect(context.Background())

	// Rotate the log by truncating; the collector must re-baseline instead of
	// producing a spurious rate from the shrunken offset.
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	appendAccessLog(t, logPath, 200, 200)
	current = current.Add(30 * time.Second)
	if rotated := collector.Collect(context.Background()); rotated.ErrorRatePercent != 0 {
		t.Fatalf("rotated window error rate = %v, want 0 baseline", rotated.ErrorRatePercent)
	}

	appendAccessLog(t, logPath, 500, 500)
	current = current.Add(30 * time.Second)
	if resumed := collector.Collect(context.Background()); !approxEqual(resumed.ErrorRatePercent, 100) {
		t.Fatalf("resumed error rate = %v, want 100", resumed.ErrorRatePercent)
	}
}

func TestCountAccessLogFailuresEdgeJSON(t *testing.T) {
	// Matches the edge_json log_format emitted by the worker config compiler.
	data := []byte(
		`{"time":"2026-07-12T00:00:00+00:00","request_id":"a","remote_addr":"1.2.3.4","host":"x.test","request":"GET / HTTP/1.1","status":200,"bytes":10,"request_time":0.01}` + "\n" +
			`{"time":"2026-07-12T00:00:01+00:00","request_id":"b","remote_addr":"1.2.3.4","host":"x.test","request":"GET /a HTTP/1.1","status":502,"bytes":0,"request_time":0.20}` + "\n" +
			`{"time":"2026-07-12T00:00:02+00:00","request_id":"c","remote_addr":"1.2.3.4","host":"x.test","request":"GET /b HTTP/1.1","status":404,"bytes":5,"request_time":0.02}` + "\n",
	)
	total, failures := countAccessLogFailures(data)
	if total != 3 || failures != 1 {
		t.Fatalf("total=%d failures=%d, want total=3 failures=1", total, failures)
	}
}

func TestParseStubStatusRequests(t *testing.T) {
	body := []byte("Active connections: 43 \nserver accepts handled requests\n 100 100 10993 \nReading: 0 Writing: 5 Waiting: 38 \n")
	got, err := parseStubStatusRequests(body)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10993 {
		t.Fatalf("requests = %v, want 10993", got)
	}
}

func TestCollectWithoutSources(t *testing.T) {
	collector := NewCollector(Options{DataDir: t.TempDir(), ProcRoot: t.TempDir()})
	snapshot := collector.Collect(context.Background())
	if snapshot.CollectedAt.IsZero() {
		t.Fatal("CollectedAt was not set")
	}
	if snapshot.CPUPercent != 0 ||
		snapshot.MemoryPercent != 0 ||
		snapshot.BandwidthInMbps != 0 ||
		snapshot.BandwidthOutMbps != 0 ||
		snapshot.RequestsPerSecond != 0 ||
		snapshot.ErrorRatePercent != 0 {
		t.Fatalf("expected zeroed metrics without sources, got %#v", snapshot)
	}
}
