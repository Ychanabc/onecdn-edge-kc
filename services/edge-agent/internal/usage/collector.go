// Package usage collects durable, at-least-once egress usage from the Nginx
// JSON access log and prepares control-plane usage reports.
//
// Log rotation / truncate handling is intentionally conservative:
//
//   - When the tracked inode/device changes, or the file size shrinks below the
//     committed offset (truncate), the collector starts reading the new file
//     from offset 0.
//   - A pending report is always resent before any cursor switch, so crash or
//     5xx during upload never drops already-prepared bytes.
//   - Rename rotation has a small loss window: if the collector cannot open the
//     previous inode after rename (common when only the new path is visible),
//     bytes written to the old file between the last committed offset and the
//     rename are not recoverable. This package does not pretend that case is
//     lossless.
package usage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/cdn-console/edge-agent/internal/control"
)

const (
	maxBatchBytes = 4 * 1024 * 1024
	maxSites      = 500
	maxLineBytes  = 1024 * 1024
)

// Options configures a Collector.
type Options struct {
	AccessLogPath string
	StateDir      string
	NodeID        string
	Now           func() time.Time
}

// Collector tails the access log with a durable cursor and pending report.
type Collector struct {
	logPath   string
	statePath string
	nodeID    string
	now       func() time.Time
}

// NewCollector builds a durable usage collector. State is stored under
// StateDir/usage-cursor.json via atomic temp+rename.
func NewCollector(options Options) *Collector {
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Collector{
		logPath:   options.AccessLogPath,
		statePath: filepath.Join(options.StateDir, "usage-cursor.json"),
		nodeID:    options.NodeID,
		now:       now,
	}
}

type accessLogLine struct {
	Timestamp   string  `json:"timestamp"`
	TenantID    string  `json:"tenant_id"`
	SiteID      string  `json:"site_id"`
	BytesSent   int64   `json:"bytes_sent"`
	RequestTime float64 `json:"request_time"`
	Status      int     `json:"status"`
}

type siteAgg struct {
	egressBytes  int64
	requestCount int64
}

// Prepare returns a usage report ready for POST /edge/v1/usage.
// If a pending report already exists it is returned unchanged (at-least-once).
// A nil report means there is nothing to upload yet.
func (collector *Collector) Prepare(ctx context.Context) (*control.UsageReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if collector.logPath == "" {
		return nil, nil
	}
	state, err := collector.loadState()
	if err != nil {
		return nil, err
	}
	if state.Pending != nil {
		report := state.Pending.Report
		return &report, nil
	}

	report, next, advanced, err := collector.readBatch(state.Committed)
	if err != nil {
		return nil, err
	}
	if !advanced {
		return nil, nil
	}
	if report == nil {
		// Consumed only malformed/empty complete lines; advance committed cursor
		// without creating a pending upload.
		state.Committed = next
		if err := collector.saveState(state); err != nil {
			return nil, err
		}
		return nil, nil
	}

	state.Pending = &Pending{Report: *report, NextCursor: next}
	if err := collector.saveState(state); err != nil {
		return nil, err
	}
	return report, nil
}

// Ack advances the committed cursor past a successfully uploaded pending report.
func (collector *Collector) Ack(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := collector.loadState()
	if err != nil {
		return err
	}
	if state.Pending == nil {
		return errors.New("usage ack without pending report")
	}
	state.Committed = state.Pending.NextCursor
	state.Pending = nil
	return collector.saveState(state)
}

func (collector *Collector) readBatch(committed Cursor) (*control.UsageReport, Cursor, bool, error) {
	info, err := os.Stat(collector.logPath)
	if os.IsNotExist(err) {
		return nil, committed, false, nil
	}
	if err != nil {
		return nil, Cursor{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, Cursor{}, false, errors.New("access log stat missing device/inode")
	}
	size := info.Size()
	device := uint64(stat.Dev)
	inode := stat.Ino

	start := committed
	// Rotation / truncate: inode/device changed, or size shrank below offset.
	// Rename rotation limitation: if the old inode is no longer reachable via
	// the configured path, bytes between the last committed offset and the
	// rename are unrecoverable; we deliberately start the new file at 0 rather
	// than pretend no loss occurred.
	rotated := (start.Device != 0 || start.Inode != 0) &&
		(start.Device != device || start.Inode != inode || size < start.Offset)
	if rotated {
		start = Cursor{Device: device, Inode: inode, Offset: 0}
	} else if start.Device == 0 && start.Inode == 0 {
		start = Cursor{Device: device, Inode: inode, Offset: 0}
	}
	if size == start.Offset {
		return nil, start, false, nil
	}

	toRead := size - start.Offset
	if toRead > maxBatchBytes {
		toRead = maxBatchBytes
	}

	file, err := os.Open(collector.logPath)
	if err != nil {
		return nil, Cursor{}, false, err
	}
	defer file.Close()
	if _, err := file.Seek(start.Offset, io.SeekStart); err != nil {
		return nil, Cursor{}, false, err
	}
	chunk := make([]byte, toRead)
	n, err := io.ReadFull(file, chunk)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, Cursor{}, false, err
	}
	chunk = chunk[:n]

	newline := bytes.LastIndexByte(chunk, '\n')
	if newline < 0 {
		// Partial trailing line only; keep offset so it is re-read next time.
		return nil, start, false, nil
	}
	complete := chunk[:newline+1]

	sites, windowStart, windowEnd, consumed, err := aggregateComplete(complete)
	if err != nil {
		return nil, Cursor{}, false, err
	}
	next := Cursor{Device: device, Inode: inode, Offset: start.Offset + consumed}
	if len(sites) == 0 {
		if consumed == 0 {
			return nil, start, false, nil
		}
		return nil, next, true, nil
	}

	reportID, err := uuidV4()
	if err != nil {
		return nil, Cursor{}, false, err
	}
	report := &control.UsageReport{
		ReportID:    reportID,
		NodeID:      collector.nodeID,
		ReportedAt:  collector.now().UTC(),
		WindowStart: windowStart.UTC(),
		WindowEnd:   windowEnd.UTC(),
		Sites:       sites,
	}
	return report, next, true, nil
}

func aggregateComplete(data []byte) ([]control.UsageSite, time.Time, time.Time, int64, error) {
	aggregates := make(map[string]*siteAgg)
	var windowStart, windowEnd time.Time
	var consumed int64
	offset := 0
	for offset < len(data) {
		rel := bytes.IndexByte(data[offset:], '\n')
		if rel < 0 {
			break
		}
		lineEnd := offset + rel + 1
		line := bytes.TrimSpace(data[offset : offset+rel])
		lineLen := int64(lineEnd - offset)

		if len(line) == 0 {
			consumed += lineLen
			offset = lineEnd
			continue
		}
		if len(line) > maxLineBytes {
			consumed += lineLen
			offset = lineEnd
			continue
		}

		parsed, ok := parseAccessLine(line)
		if !ok {
			consumed += lineLen
			offset = lineEnd
			continue
		}

		if _, exists := aggregates[parsed.SiteID]; !exists && len(aggregates) >= maxSites {
			// Stop before this line so the next Prepare continues here.
			break
		}
		agg := aggregates[parsed.SiteID]
		if agg == nil {
			agg = &siteAgg{}
			aggregates[parsed.SiteID] = agg
		}
		agg.egressBytes += parsed.BytesSent
		agg.requestCount++
		if windowStart.IsZero() || parsed.Timestamp.Before(windowStart) {
			windowStart = parsed.Timestamp
		}
		if windowEnd.IsZero() || parsed.Timestamp.After(windowEnd) {
			windowEnd = parsed.Timestamp
		}
		consumed += lineLen
		offset = lineEnd
	}

	if len(aggregates) == 0 {
		return nil, time.Time{}, time.Time{}, consumed, nil
	}

	sites := make([]control.UsageSite, 0, len(aggregates))
	for siteID, agg := range aggregates {
		sites = append(sites, control.UsageSite{
			SiteID:       siteID,
			EgressBytes:  agg.egressBytes,
			RequestCount: agg.requestCount,
		})
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].SiteID < sites[j].SiteID
	})
	return sites, windowStart, windowEnd, consumed, nil
}

type parsedLine struct {
	SiteID    string
	BytesSent int64
	Timestamp time.Time
}

func parseAccessLine(line []byte) (parsedLine, bool) {
	var entry accessLogLine
	if err := json.Unmarshal(line, &entry); err != nil {
		return parsedLine{}, false
	}
	if entry.SiteID == "" || entry.BytesSent < 0 {
		return parsedLine{}, false
	}
	timestamp, err := parseTimestamp(entry.Timestamp)
	if err != nil {
		return parsedLine{}, false
	}
	return parsedLine{
		SiteID:    entry.SiteID,
		BytesSent: entry.BytesSent,
		Timestamp: timestamp,
	}, true
}

func parseTimestamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, errors.New("missing timestamp")
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func uuidV4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate report id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:]), nil
}
