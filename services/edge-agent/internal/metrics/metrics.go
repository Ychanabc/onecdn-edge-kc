// Package metrics collects real node telemetry for the edge agent report loop.
//
// The agent shares OpenResty's network and PID namespaces, so host-level
// counters exposed through /proc reflect this dedicated edge node, and the
// loopback stub_status endpoint reports the data-plane request counter. All
// readings are best-effort: a source that is missing or unreadable yields 0 for
// that field instead of failing the whole report.
package metrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cdn-console/edge-agent/internal/control"
)

const (
	statusResponseLimit = 4096
	accessLogChunkLimit = 8 * 1024 * 1024
	accessLogLineLimit  = 1024 * 1024
)

// Options configures a Collector.
type Options struct {
	// DataDir is the volume used for disk utilisation via statfs.
	DataDir string
	// StatusURL is the nginx stub_status endpoint. A nil URL disables the
	// requests-per-second reading.
	StatusURL *url.URL
	// AccessLogPath is a JSON access log used to derive the error rate. An empty
	// path disables the error-rate reading.
	AccessLogPath string
	// HTTPTimeout bounds the stub_status request. Defaults to 5s.
	HTTPTimeout time.Duration
	// ProcRoot overrides the /proc mount point. Used by tests.
	ProcRoot string
	// Now overrides the clock. Used by tests.
	Now func() time.Time
	// HTTPClient overrides the HTTP client. Used by tests.
	HTTPClient *http.Client
}

type cpuSample struct {
	total float64
	idle  float64
}

type netSample struct {
	rxBytes float64
	txBytes float64
	at      time.Time
}

type reqSample struct {
	requests float64
	at       time.Time
}

type logTailState struct {
	device uint64
	inode  uint64
	offset int64
	carry  []byte
}

// Collector gathers node metrics and retains the previous sample so that
// rate-based fields (CPU, bandwidth, requests, error rate) can be derived from
// deltas between successive Collect calls.
type Collector struct {
	dataDir       string
	statusURL     *url.URL
	accessLogPath string
	procRoot      string
	now           func() time.Time
	httpClient    *http.Client

	mu       sync.Mutex
	cpuPrev  *cpuSample
	netPrev  *netSample
	reqPrev  *reqSample
	logState logTailState
}

// NewCollector builds a Collector from the supplied options, applying defaults
// for the clock, proc mount, and HTTP client.
func NewCollector(options Options) *Collector {
	procRoot := options.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	client := options.HTTPClient
	if client == nil {
		timeout := options.HTTPTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		client = &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("status endpoint redirects are forbidden")
			},
			Timeout:   timeout,
			Transport: &http.Transport{Proxy: nil},
		}
	}
	return &Collector{
		accessLogPath: options.AccessLogPath,
		dataDir:       options.DataDir,
		httpClient:    client,
		now:           now,
		procRoot:      procRoot,
		statusURL:     options.StatusURL,
	}
}

// Collect samples every source once and returns a NodeMetrics snapshot. It never
// returns an error: unavailable sources contribute 0 so a partially observable
// node still reports.
func (collector *Collector) Collect(ctx context.Context) control.NodeMetrics {
	collector.mu.Lock()
	defer collector.mu.Unlock()

	now := collector.now().UTC()
	inMbps, outMbps := collector.bandwidthMbps(now)
	return control.NodeMetrics{
		BandwidthInMbps:   inMbps,
		BandwidthOutMbps:  outMbps,
		CollectedAt:       now,
		CPUPercent:        collector.cpuPercent(),
		DiskPercent:       collector.diskPercent(),
		ErrorRatePercent:  collector.errorRatePercent(),
		MemoryPercent:     collector.memoryPercent(),
		RequestsPerSecond: collector.requestsPerSecond(ctx, now),
	}
}

func (collector *Collector) cpuPercent() float64 {
	total, idle, err := readCPU(filepath.Join(collector.procRoot, "stat"))
	if err != nil {
		return 0
	}
	previous := collector.cpuPrev
	collector.cpuPrev = &cpuSample{idle: idle, total: total}
	if previous == nil {
		return 0
	}
	deltaTotal := total - previous.total
	deltaIdle := idle - previous.idle
	if deltaTotal <= 0 || deltaIdle < 0 {
		return 0
	}
	return clampPercent((deltaTotal - deltaIdle) / deltaTotal * 100)
}

func (collector *Collector) memoryPercent() float64 {
	values, err := readMeminfo(filepath.Join(collector.procRoot, "meminfo"))
	if err != nil {
		return 0
	}
	total := values["MemTotal"]
	if total <= 0 {
		return 0
	}
	available, ok := values["MemAvailable"]
	if !ok {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	used := total - available
	if used < 0 {
		used = 0
	}
	return clampPercent(used / total * 100)
}

func (collector *Collector) diskPercent() float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(collector.dataDir, &stat); err != nil || stat.Blocks == 0 {
		return 0
	}
	return clampPercent((1 - float64(stat.Bavail)/float64(stat.Blocks)) * 100)
}

func (collector *Collector) bandwidthMbps(now time.Time) (float64, float64) {
	rx, tx, err := readNetDev(filepath.Join(collector.procRoot, "net", "dev"))
	if err != nil {
		return 0, 0
	}
	previous := collector.netPrev
	collector.netPrev = &netSample{at: now, rxBytes: rx, txBytes: tx}
	if previous == nil {
		return 0, 0
	}
	elapsed := now.Sub(previous.at).Seconds()
	deltaRx := rx - previous.rxBytes
	deltaTx := tx - previous.txBytes
	if elapsed <= 0 || deltaRx < 0 || deltaTx < 0 {
		return 0, 0
	}
	return nonNegative(deltaRx * 8 / 1e6 / elapsed), nonNegative(deltaTx * 8 / 1e6 / elapsed)
}

func (collector *Collector) requestsPerSecond(ctx context.Context, now time.Time) float64 {
	if collector.statusURL == nil {
		return 0
	}
	requests, err := collector.readStubStatus(ctx)
	if err != nil {
		return 0
	}
	previous := collector.reqPrev
	collector.reqPrev = &reqSample{at: now, requests: requests}
	if previous == nil {
		return 0
	}
	elapsed := now.Sub(previous.at).Seconds()
	delta := requests - previous.requests
	if elapsed <= 0 || delta < 0 {
		return 0
	}
	return nonNegative(delta / elapsed)
}

func (collector *Collector) readStubStatus(ctx context.Context) (float64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, collector.statusURL.String(), nil)
	if err != nil {
		return 0, err
	}
	response, err := collector.httpClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("stub_status returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, statusResponseLimit))
	if err != nil {
		return 0, err
	}
	return parseStubStatusRequests(body)
}

func (collector *Collector) errorRatePercent() float64 {
	if collector.accessLogPath == "" {
		return 0
	}
	total, failures, ok := collector.readAccessLogDelta()
	if !ok || total == 0 {
		return 0
	}
	return clampPercent(float64(failures) / float64(total) * 100)
}

func (collector *Collector) readAccessLogDelta() (total int, failures int, ok bool) {
	info, err := os.Stat(collector.accessLogPath)
	if err != nil {
		collector.logState = logTailState{}
		return 0, 0, false
	}
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !statOK {
		return 0, 0, false
	}
	size := info.Size()
	state := collector.logState
	firstObservation := state.device == 0 && state.inode == 0
	rotated := state.device != uint64(stat.Dev) || state.inode != stat.Ino || size < state.offset
	if firstObservation || rotated {
		// Establish the baseline at the current end so pre-existing history is
		// not counted into the first window.
		collector.logState = logTailState{device: uint64(stat.Dev), inode: stat.Ino, offset: size}
		return 0, 0, false
	}
	if size == state.offset {
		return 0, 0, false
	}

	file, err := os.Open(collector.accessLogPath)
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()
	if _, err := file.Seek(state.offset, io.SeekStart); err != nil {
		return 0, 0, false
	}
	limit := size - state.offset
	if limit > accessLogChunkLimit {
		limit = accessLogChunkLimit
	}
	chunk := make([]byte, limit)
	read, err := io.ReadFull(file, chunk)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, 0, false
	}
	chunk = chunk[:read]

	data := append(collector.logState.carry, chunk...)
	newline := bytes.LastIndexByte(data, '\n')
	complete := data
	if newline >= 0 {
		complete = data[:newline+1]
		state.carry = append([]byte(nil), data[newline+1:]...)
	} else {
		complete = nil
		state.carry = data
	}
	state.offset += int64(read)
	state.device = uint64(stat.Dev)
	state.inode = stat.Ino
	collector.logState = state

	total, failures = countAccessLogFailures(complete)
	return total, failures, true
}

func countAccessLogFailures(data []byte) (total int, failures int) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), accessLogLineLimit)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Status int `json:"status"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		total++
		if entry.Status >= 500 {
			failures++
		}
	}
	return total, failures
}

func readCPU(path string) (total float64, idle float64, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var sum, idleTicks float64
		for index, field := range fields[1:] {
			value, parseErr := strconv.ParseFloat(field, 64)
			if parseErr != nil {
				return 0, 0, parseErr
			}
			sum += value
			if index == 3 || index == 4 {
				idleTicks += value
			}
		}
		return sum, idleTicks, nil
	}
	return 0, 0, errors.New("cpu aggregate line not found")
}

func readMeminfo(path string) (map[string]float64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]float64)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, parseErr := strconv.ParseFloat(fields[1], 64)
		if parseErr != nil {
			continue
		}
		values[key] = value
	}
	return values, scanner.Err()
}

func readNetDev(path string) (rx float64, tx float64, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		rxBytes, rxErr := strconv.ParseFloat(fields[0], 64)
		txBytes, txErr := strconv.ParseFloat(fields[8], 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		rx += rxBytes
		tx += txBytes
	}
	return rx, tx, scanner.Err()
}

func parseStubStatusRequests(body []byte) (float64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		accepts, acceptsErr := strconv.ParseFloat(fields[0], 64)
		handled, handledErr := strconv.ParseFloat(fields[1], 64)
		requests, requestsErr := strconv.ParseFloat(fields[2], 64)
		if acceptsErr == nil && handledErr == nil && requestsErr == nil {
			_ = accepts
			_ = handled
			return requests, nil
		}
	}
	return 0, errors.New("stub_status requests counter not found")
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func nonNegative(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}
