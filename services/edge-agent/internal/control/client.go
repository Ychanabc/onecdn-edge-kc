package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxJSONResponseBytes = 4 * 1024 * 1024

type Client struct {
	baseURL          *url.URL
	httpClient       *http.Client
	artifactMaxBytes int64
}

type envelope[T any] struct {
	Code    int             `json:"code"`
	Data    T               `json:"data"`
	Error   *string         `json:"error"`
	Message string          `json:"message"`
	Meta    json.RawMessage `json:"meta"`
}

func certPool(path string, useSystem bool) (*x509.CertPool, error) {
	var pool *x509.CertPool
	var err error
	if useSystem {
		pool, err = x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system certificate pool: %w", err)
		}
	} else {
		pool = x509.NewCertPool()
	}
	if path == "" {
		return pool, nil
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("CA bundle contains no valid certificates")
	}
	return pool, nil
}

func transport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: 20 * time.Second,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
	}
}

func NewBootstrapClient(baseURL *url.URL, caFile string, artifactMaxBytes int64) (*Client, error) {
	roots, err := certPool(caFile, true)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: transport(&tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
			}),
		},
		artifactMaxBytes: artifactMaxBytes,
	}, nil
}

func NewMTLSClient(baseURL *url.URL, certificateFile, keyFile, caFile string, artifactMaxBytes int64) (*Client, error) {
	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load mTLS client identity: %w", err)
	}
	roots, err := certPool(caFile, false)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
			Transport: transport(&tls.Config{
				Certificates: []tls.Certificate{certificate},
				MinVersion:   tls.VersionTLS12,
				RootCAs:      roots,
			}),
		},
		artifactMaxBytes: artifactMaxBytes,
	}, nil
}

func (client *Client) endpoint(path string) (*url.URL, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse control-plane path: %w", err)
	}
	if reference.IsAbs() || reference.Host != "" {
		return nil, errors.New("control-plane endpoint must be relative")
	}
	resolved := client.baseURL.ResolveReference(reference)
	if resolved.Scheme != "https" || !strings.EqualFold(resolved.Host, client.baseURL.Host) {
		return nil, errors.New("control-plane endpoint escaped configured origin")
	}
	return resolved, nil
}

func decodeEnvelope[T any](response *http.Response) (T, error) {
	var zero T
	limited := io.LimitReader(response.Body, maxJSONResponseBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return zero, fmt.Errorf("read control-plane response: %w", err)
	}
	if len(content) > maxJSONResponseBytes {
		return zero, errors.New("control-plane JSON response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return zero, fmt.Errorf("control-plane returned HTTP %d", response.StatusCode)
	}
	var result envelope[T]
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return zero, fmt.Errorf("decode control-plane response: %w", err)
	}
	if result.Code != 0 || result.Error != nil {
		if result.Error != nil {
			return zero, fmt.Errorf("control-plane error: %s", *result.Error)
		}
		return zero, fmt.Errorf("control-plane error code: %d", result.Code)
	}
	return result.Data, nil
}

func (client *Client) doJSON(ctx context.Context, method, path string, requestBody, responseBody any) error {
	endpoint, err := client.endpoint(path)
	if err != nil {
		return err
	}
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("control-plane request failed: %w", err)
	}
	defer response.Body.Close()

	switch target := responseBody.(type) {
	case *BootstrapResponse:
		decoded, err := decodeEnvelope[BootstrapResponse](response)
		if err == nil {
			*target = decoded
		}
		return err
	case nil:
		_, err := decodeEnvelope[json.RawMessage](response)
		return err
	default:
		return errors.New("unsupported JSON response target")
	}
}

func (client *Client) Bootstrap(ctx context.Context, request BootstrapRequest) (BootstrapResponse, error) {
	var response BootstrapResponse
	err := client.doJSON(ctx, http.MethodPost, "/edge/v1/bootstrap", request, &response)
	return response, err
}

func (client *Client) Desired(ctx context.Context, etag string) (*DesiredConfig, bool, error) {
	endpoint, err := client.endpoint("/edge/v1/desired-config")
	if err != nil {
		return nil, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("create desired-state request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("pull desired state: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	desired, err := decodeEnvelope[DesiredConfig](response)
	if err != nil {
		return nil, false, err
	}
	return &desired, false, nil
}

func (client *Client) DownloadArtifact(ctx context.Context, path string, expectedSize int64) ([]byte, error) {
	if expectedSize < 0 || expectedSize > client.artifactMaxBytes {
		return nil, errors.New("artifact metadata exceeds configured size limit")
	}
	endpoint, err := client.endpoint(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(endpoint.Path, "/edge/v1/artifacts/") {
		return nil, errors.New("artifact URL is outside the artifact endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create artifact request: %w", err)
	}
	request.Header.Set("Accept", "application/x-tar")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifact endpoint returned HTTP %d", response.StatusCode)
	}
	if contentLength := response.Header.Get("Content-Length"); contentLength != "" {
		size, err := strconv.ParseInt(contentLength, 10, 64)
		if err != nil || size != expectedSize || size > client.artifactMaxBytes {
			return nil, errors.New("artifact Content-Length does not match signed metadata")
		}
	}
	limited := io.LimitReader(response.Body, client.artifactMaxBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if int64(len(content)) > client.artifactMaxBytes || int64(len(content)) != expectedSize {
		return nil, errors.New("downloaded artifact size does not match signed metadata")
	}
	return content, nil
}

func (client *Client) Report(ctx context.Context, report Report) error {
	return client.doJSON(ctx, http.MethodPost, "/edge/v1/report", report, nil)
}

func (client *Client) Ack(ctx context.Context, ack Ack) error {
	return client.doJSON(ctx, http.MethodPost, "/edge/v1/ack", ack, nil)
}

func (client *Client) Usage(ctx context.Context, report UsageReport) error {
	return client.doJSON(ctx, http.MethodPost, "/edge/v1/usage", report, nil)
}

func (client *Client) AccessEvents(ctx context.Context, batch AccessEventBatch) error {
	return client.doJSON(ctx, http.MethodPost, "/edge/v1/access-events", batch, nil)
}
