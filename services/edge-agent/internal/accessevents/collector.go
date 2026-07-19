package accessevents

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/cdn-console/edge-agent/internal/control"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type state struct {
	Offset  int64    `json:"offset"`
	Pending *pending `json:"pending,omitempty"`
}
type pending struct {
	Batch control.AccessEventBatch `json:"batch"`
	Next  int64                    `json:"next"`
}
type Collector struct{ logPath, statePath, nodeID string }

func New(logPath, stateDir, nodeID string) *Collector {
	return &Collector{logPath: logPath, statePath: filepath.Join(stateDir, "access-events-cursor.json"), nodeID: nodeID}
}
func (c *Collector) load() (state, error) {
	var s state
	b, e := os.ReadFile(c.statePath)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	e = json.Unmarshal(b, &s)
	return s, e
}
func (c *Collector) save(s state) error {
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(filepath.Dir(c.statePath), ".access-events-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0600)
	if _, e = tmp.Write(append(b, '\n')); e == nil {
		e = tmp.Sync()
	}
	if ce := tmp.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(name, c.statePath)
}
func id() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

type line struct {
	Timestamp        string  `json:"timestamp"`
	SiteID           string  `json:"site_id"`
	RequestID        string  `json:"request_id"`
	Method           string  `json:"method"`
	URI              string  `json:"uri"`
	Status           int     `json:"status"`
	Bytes            int64   `json:"bytes_sent"`
	RequestTime      float64 `json:"request_time"`
	UpstreamAddress  string  `json:"upstream_addr"`
	UpstreamConnect  string  `json:"upstream_connect_time"`
	UpstreamHeader   string  `json:"upstream_header_time"`
	UpstreamResponse string  `json:"upstream_response_time"`
	UpstreamRetries  int     `json:"upstream_retries"`
	Cache            string  `json:"cache_status"`
	WAF              string  `json:"waf_action"`
	CC               string  `json:"cc_action"`
	AttackType       string  `json:"attack_type"`
	UA               string  `json:"ua_category"`
	IP               string  `json:"remote_addr"`
	Country          string  `json:"country"`
	ASN              int64   `json:"asn"`
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func milliseconds(value string) *int {
	for _, part := range strings.Split(value, ",") {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err == nil && n >= 0 {
			v := int(n * 1000)
			return &v
		}
	}
	return nil
}
func intptr(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
func (c *Collector) Prepare(ctx context.Context) (*control.AccessEventBatch, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s, e := c.load()
	if e != nil {
		return nil, e
	}
	if s.Pending != nil {
		return &s.Pending.Batch, nil
	}
	f, e := os.Open(c.logPath)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if _, e = f.Seek(s.Offset, 0); e != nil {
		return nil, e
	}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 1024*1024)
	events := make([]control.AccessEvent, 0, 500)
	next := s.Offset
	for scan.Scan() {
		next += int64(len(scan.Bytes()) + 1)
		var l line
		if json.Unmarshal(scan.Bytes(), &l) != nil || l.SiteID == "" || l.RequestID == "" {
			continue
		}
		t, e := time.Parse(time.RFC3339, l.Timestamp)
		if e != nil {
			continue
		}
		u, e := url.ParseRequestURI(l.URI)
		if e != nil || !strings.HasPrefix(u.Path, "/") {
			continue
		}
		eid, e := id()
		if e != nil {
			return nil, e
		}
		cacheStatus := strings.ToLower(l.Cache)
		if cacheStatus == "" {
			cacheStatus = "unknown"
		}
		wafAction := strings.ToLower(l.WAF)
		if wafAction == "" {
			wafAction = "none"
		}
		uaCategory := strings.ToLower(l.UA)
		if uaCategory == "" {
			uaCategory = "unknown"
		}
		country := strings.ToUpper(l.Country)
		ccAction := strings.ToLower(l.CC)
		if ccAction == "" {
			ccAction = "none"
		}
		events = append(events, control.AccessEvent{EventID: eid, OccurredAt: t, SiteID: l.SiteID, RequestID: l.RequestID, Method: strings.ToUpper(l.Method), Path: u.Path, Status: l.Status, BytesSent: l.Bytes, LatencyMs: int(l.RequestTime * 1000), UpstreamAddress: ptr(l.UpstreamAddress), UpstreamConnectMs: milliseconds(l.UpstreamConnect), UpstreamHeaderMs: milliseconds(l.UpstreamHeader), UpstreamResponseMs: milliseconds(l.UpstreamResponse), UpstreamRetryCount: l.UpstreamRetries, CacheStatus: cacheStatus, WAFAction: wafAction, CCAction: ccAction, AttackType: ptr(l.AttackType), UserAgentCategory: uaCategory, SourceIP: ptr(l.IP), Country: ptr(country), ASN: intptr(l.ASN)})
		if len(events) >= 500 {
			break
		}
	}
	if e := scan.Err(); e != nil {
		return nil, e
	}
	if len(events) == 0 {
		if next != s.Offset {
			s.Offset = next
			_ = c.save(s)
		}
		return nil, nil
	}
	bid, e := id()
	if e != nil {
		return nil, e
	}
	batch := control.AccessEventBatch{BatchID: bid, NodeID: c.nodeID, SentAt: time.Now().UTC(), Events: events}
	s.Pending = &pending{Batch: batch, Next: next}
	if e = c.save(s); e != nil {
		return nil, e
	}
	return &batch, nil
}
func (c *Collector) Ack(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	s, e := c.load()
	if e != nil {
		return e
	}
	if s.Pending == nil {
		return fmt.Errorf("access event ack without pending batch")
	}
	s.Offset = s.Pending.Next
	s.Pending = nil
	return c.save(s)
}
