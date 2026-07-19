package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func desiredTestClient(t *testing.T, body string) *Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{baseURL: baseURL, httpClient: server.Client(), artifactMaxBytes: 1024}
}

func TestDesiredDecodesCertificatesContract(t *testing.T) {
	body := `{"code":0,"data":{"certificates":[{"certificateId":"10000000-0000-4000-8000-000000000010","certificateSecretRef":"secret://certificate","deploymentId":"10000000-0000-4000-8000-000000000011","domains":["cdn.example.com"],"expiresAt":"2027-01-01T00:00:00Z","privateKeySecretRef":"secret://private-key"}],"command":null,"configs":[],"etag":"empty-v1","generatedAt":"2026-07-17T00:00:00Z","generation":"empty-v1","nodeId":"10000000-0000-4000-8000-000000000001"},"error":null,"message":"ok","meta":null}`
	desired, notModified, err := desiredTestClient(t, body).Desired(context.Background(), "")
	if err != nil || notModified {
		t.Fatalf("Desired() = %#v, %v, %v", desired, notModified, err)
	}
	if len(desired.Certificates) != 1 || desired.Certificates[0].PrivateKeySecretRef != "secret://private-key" {
		t.Fatalf("certificates = %#v", desired.Certificates)
	}
}

func TestDesiredRejectsUnknownCertificateField(t *testing.T) {
	body := `{"code":0,"data":{"certificates":[{"certificateId":"10000000-0000-4000-8000-000000000010","certificateSecretRef":"secret://certificate","deploymentId":"10000000-0000-4000-8000-000000000011","domains":["cdn.example.com"],"expiresAt":"2027-01-01T00:00:00Z","privateKeySecretRef":"secret://private-key","unexpected":true}],"command":null,"configs":[],"etag":"empty-v1","generatedAt":"2026-07-17T00:00:00Z","generation":"empty-v1","nodeId":"10000000-0000-4000-8000-000000000001"},"error":null,"message":"ok","meta":null}`
	_, _, err := desiredTestClient(t, body).Desired(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
}
