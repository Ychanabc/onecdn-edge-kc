package control

import "time"

type SigningTrustKey struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

type BootstrapRequest struct {
	AgentVersion   string `json:"agentVersion"`
	BootstrapToken string `json:"bootstrapToken"`
	CSRPEM         string `json:"csrPem"`
	Hostname       string `json:"hostname"`
}

type BootstrapResponse struct {
	ClientCertificatePEM       string            `json:"clientCertificatePem"`
	ClientCertificateExpiresAt time.Time         `json:"clientCertificateExpiresAt"`
	NodeID                     string            `json:"nodeId"`
	ServerCACertificatePEM     string            `json:"serverCaCertificatePem"`
	TenantID                   string            `json:"tenantId"`
	TrustBundle                []SigningTrustKey `json:"trustBundle"`
}

type ArtifactManifest struct {
	Files           map[string]string `json:"files"`
	Generation      string            `json:"generation"`
	MinAgentVersion string            `json:"minAgentVersion"`
	SigningKeyID    string            `json:"signingKeyId"`
	SiteID          string            `json:"siteId"`
	TenantID        string            `json:"tenantId"`
	Version         int               `json:"version"`
}

type ArtifactMetadata struct {
	CreatedAt    time.Time        `json:"createdAt"`
	Digest       string           `json:"digest"`
	Key          string           `json:"key"`
	KeyID        string           `json:"keyId"`
	Manifest     ArtifactManifest `json:"manifest"`
	ManifestPath string           `json:"manifestPath"`
	Signature    string           `json:"signature"`
	SizeBytes    int64            `json:"sizeBytes"`
}

type DesiredSiteConfig struct {
	Artifact     ArtifactMetadata `json:"artifact"`
	DeploymentID *string          `json:"deploymentId"`
	Domain       string           `json:"domain"`
	DownloadURL  string           `json:"downloadUrl"`
	Revision     int              `json:"revision"`
	RevisionID   string           `json:"revisionId"`
	SiteID       string           `json:"siteId"`
}

type Command struct {
	ID       string    `json:"id"`
	IssuedAt time.Time `json:"issuedAt"`
	Type     string    `json:"type"`
}

type DesiredCertificate struct {
	CertificateID        string    `json:"certificateId"`
	CertificateSecretRef string    `json:"certificateSecretRef"`
	DeploymentID         string    `json:"deploymentId"`
	Domains              []string  `json:"domains"`
	ExpiresAt            time.Time `json:"expiresAt"`
	PrivateKeySecretRef  string    `json:"privateKeySecretRef"`
}

type DesiredConfig struct {
	Certificates []DesiredCertificate `json:"certificates"`
	Command      *Command             `json:"command"`
	Configs      []DesiredSiteConfig  `json:"configs"`
	ETag         string               `json:"etag"`
	GeneratedAt  time.Time            `json:"generatedAt"`
	Generation   string               `json:"generation"`
	NodeID       string               `json:"nodeId"`
}

type NodeMetrics struct {
	BandwidthInMbps   float64   `json:"bandwidthInMbps"`
	BandwidthOutMbps  float64   `json:"bandwidthOutMbps"`
	CollectedAt       time.Time `json:"collectedAt"`
	CPUPercent        float64   `json:"cpuPercent"`
	DiskPercent       float64   `json:"diskPercent"`
	ErrorRatePercent  float64   `json:"errorRatePercent"`
	MemoryPercent     float64   `json:"memoryPercent"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
}

type Report struct {
	AppliedGeneration *string     `json:"appliedGeneration"`
	Errors            []string    `json:"errors"`
	Metrics           NodeMetrics `json:"metrics"`
	NodeID            string      `json:"nodeId"`
	ReportedAt        time.Time   `json:"reportedAt"`
	Status            string      `json:"status"`
	Version           string      `json:"version"`
}

type Ack struct {
	AppliedAt  time.Time `json:"appliedAt"`
	CommandID  *string   `json:"commandId"`
	Errors     []string  `json:"errors"`
	Generation string    `json:"generation"`
	NodeID     string    `json:"nodeId"`
	Status     string    `json:"status"`
}

// UsageSite is one site's aggregated egress for a usage report window.
type UsageSite struct {
	SiteID       string `json:"siteId"`
	EgressBytes  int64  `json:"egressBytes"`
	RequestCount int64  `json:"requestCount"`
}

// UsageReport is the body for POST /edge/v1/usage.
type UsageReport struct {
	ReportID    string      `json:"reportId"`
	NodeID      string      `json:"nodeId"`
	ReportedAt  time.Time   `json:"reportedAt"`
	WindowStart time.Time   `json:"windowStart"`
	WindowEnd   time.Time   `json:"windowEnd"`
	Sites       []UsageSite `json:"sites"`
}

// AccessEvent is a privacy-controlled structured edge request event.
type AccessEvent struct {
	EventID            string    `json:"eventId"`
	OccurredAt         time.Time `json:"occurredAt"`
	SiteID             string    `json:"siteId"`
	RequestID          string    `json:"requestId"`
	Method             string    `json:"method"`
	Path               string    `json:"path"`
	Status             int       `json:"status"`
	BytesSent          int64     `json:"bytesSent"`
	LatencyMs          int       `json:"latencyMs"`
	UpstreamAddress    *string   `json:"upstreamAddress"`
	UpstreamConnectMs  *int      `json:"upstreamConnectMs"`
	UpstreamHeaderMs   *int      `json:"upstreamHeaderMs"`
	UpstreamResponseMs *int      `json:"upstreamResponseMs"`
	UpstreamRetryCount int       `json:"upstreamRetryCount"`
	CacheStatus        string    `json:"cacheStatus"`
	WAFAction          string    `json:"wafAction"`
	CCAction           string    `json:"ccAction"`
	AttackType         *string   `json:"attackType"`
	UserAgentCategory  string    `json:"userAgentCategory"`
	SourceIP           *string   `json:"sourceIp"`
	Country            *string   `json:"country"`
	ASN                *int64    `json:"asn"`
}
type AccessEventBatch struct {
	BatchID string        `json:"batchId"`
	NodeID  string        `json:"nodeId"`
	SentAt  time.Time     `json:"sentAt"`
	Events  []AccessEvent `json:"events"`
}
