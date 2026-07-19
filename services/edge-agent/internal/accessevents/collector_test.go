package accessevents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectorPreparesAndAcknowledgesAccessEvents(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "access.log")
	line := `{"timestamp":"2026-07-15T00:00:00Z","site_id":"20000000-0000-4000-8000-00000000000a","request_id":"request-1","method":"get","uri":"/products?id=secret","status":200,"bytes_sent":42,"request_time":0.125,"upstream_addr":"10.0.0.8:443","upstream_connect_time":"0.010","upstream_header_time":"0.050","upstream_response_time":"0.100","upstream_retries":1,"cache_status":"HIT","waf_action":"allow","cc_action":"block","attack_type":"cc","ua_category":"browser","remote_addr":"192.0.2.10","country":"tw","asn":3462}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	collector := New(logPath, stateDir, "10000000-0000-4000-8000-000000000001")
	batch, err := collector.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || len(batch.Events) != 1 {
		t.Fatalf("batch = %#v, want one event", batch)
	}
	event := batch.Events[0]
	if event.Path != "/products" || event.Method != "GET" || event.LatencyMs != 125 {
		t.Fatalf("event = %#v", event)
	}
	if event.UpstreamAddress == nil || *event.UpstreamAddress != "10.0.0.8:443" || event.UpstreamConnectMs == nil || *event.UpstreamConnectMs != 10 || event.UpstreamRetryCount != 1 || event.CCAction != "block" {
		t.Fatalf("upstream event = %#v", event)
	}
	if event.SourceIP == nil || *event.SourceIP != "192.0.2.10" {
		t.Fatalf("source IP = %#v", event.SourceIP)
	}

	retry, err := collector.Prepare(context.Background())
	if err != nil || retry == nil || retry.BatchID != batch.BatchID {
		t.Fatalf("retry = %#v, err = %v", retry, err)
	}
	if err := collector.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	empty, err := collector.Prepare(context.Background())
	if err != nil || empty != nil {
		t.Fatalf("after ack = %#v, err = %v", empty, err)
	}
}
