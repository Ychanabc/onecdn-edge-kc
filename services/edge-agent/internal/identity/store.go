package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cdn-console/edge-agent/internal/control"
)

type Identity struct {
	CertificateExpiresAt time.Time `json:"certificateExpiresAt"`
	NodeID               string    `json:"nodeId"`
	TenantID             string    `json:"tenantId"`
}

type Store struct {
	directory string
}

func NewStore(dataDirectory string) *Store {
	return &Store{directory: filepath.Join(dataDirectory, "identity")}
}

func (store *Store) CertificatePath() string {
	return filepath.Join(store.directory, "client.crt")
}

func (store *Store) KeyPath() string {
	return filepath.Join(store.directory, "client.key")
}

func (store *Store) CAPath() string {
	return filepath.Join(store.directory, "server-ca.crt")
}

func (store *Store) identityPath() string {
	return filepath.Join(store.directory, "identity.json")
}

func (store *Store) trustBundlePath() string {
	return filepath.Join(store.directory, "signing-keys.json")
}

func checkPrivateMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions must not grant group or other access", path)
	}
	return nil
}

func decodeStrictJSON(path string, target any) error {
	if err := checkPrivateMode(path); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func (store *Store) Load() (Identity, error) {
	var identity Identity
	if err := decodeStrictJSON(store.identityPath(), &identity); err != nil {
		return Identity{}, fmt.Errorf("load node identity: %w", err)
	}
	for _, path := range []string{
		store.CertificatePath(),
		store.KeyPath(),
		store.CAPath(),
		store.trustBundlePath(),
	} {
		if err := checkPrivateMode(path); err != nil {
			return Identity{}, fmt.Errorf("validate identity file: %w", err)
		}
	}
	if identity.NodeID == "" || identity.TenantID == "" || time.Now().After(identity.CertificateExpiresAt) {
		return Identity{}, errors.New("stored node identity is invalid or expired")
	}
	if _, err := tls.LoadX509KeyPair(store.CertificatePath(), store.KeyPath()); err != nil {
		return Identity{}, fmt.Errorf("stored mTLS key pair is invalid: %w", err)
	}
	if _, err := store.SigningKeys(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (store *Store) Exists() bool {
	_, err := os.Stat(store.identityPath())
	return err == nil
}

func atomicWrite(path string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".edge-identity-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func validateTrustBundle(keys []control.SigningTrustKey) error {
	if len(keys) == 0 {
		return errors.New("bootstrap trust bundle is empty")
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key.KeyID == "" {
			return errors.New("bootstrap trust key ID is empty")
		}
		if _, exists := seen[key.KeyID]; exists {
			return fmt.Errorf("duplicate bootstrap trust key ID: %s", key.KeyID)
		}
		raw, err := base64.StdEncoding.DecodeString(key.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return fmt.Errorf("bootstrap trust key %s is not a raw Ed25519 public key", key.KeyID)
		}
		seen[key.KeyID] = struct{}{}
	}
	return nil
}

func (store *Store) persist(response control.BootstrapResponse, privateKeyPEM []byte) (Identity, error) {
	if err := validateTrustBundle(response.TrustBundle); err != nil {
		return Identity{}, err
	}
	if response.NodeID == "" || response.TenantID == "" || response.ClientCertificateExpiresAt.Before(time.Now()) {
		return Identity{}, errors.New("bootstrap returned an invalid node identity")
	}
	if _, err := tls.X509KeyPair([]byte(response.ClientCertificatePEM), privateKeyPEM); err != nil {
		return Identity{}, fmt.Errorf("bootstrap certificate does not match generated key: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(response.ServerCACertificatePEM)) {
		return Identity{}, errors.New("bootstrap server CA is invalid")
	}
	parent := filepath.Dir(store.directory)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Identity{}, fmt.Errorf("create identity parent directory: %w", err)
	}
	stagingDirectory, err := os.MkdirTemp(parent, ".identity-staging-")
	if err != nil {
		return Identity{}, fmt.Errorf("create identity staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDirectory)
	if err := os.Chmod(stagingDirectory, 0o700); err != nil {
		return Identity{}, err
	}
	identity := Identity{
		CertificateExpiresAt: response.ClientCertificateExpiresAt,
		NodeID:               response.NodeID,
		TenantID:             response.TenantID,
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return Identity{}, err
	}
	trustBundleJSON, err := json.Marshal(response.TrustBundle)
	if err != nil {
		return Identity{}, err
	}
	files := []struct {
		content []byte
		name    string
	}{
		{privateKeyPEM, "client.key"},
		{[]byte(response.ClientCertificatePEM), "client.crt"},
		{[]byte(response.ServerCACertificatePEM), "server-ca.crt"},
		{append(identityJSON, '\n'), "identity.json"},
		{append(trustBundleJSON, '\n'), "signing-keys.json"},
	}
	for _, file := range files {
		if err := atomicWrite(filepath.Join(stagingDirectory, file.name), file.content); err != nil {
			return Identity{}, fmt.Errorf("persist identity file: %w", err)
		}
	}
	directory, err := os.Open(stagingDirectory)
	if err != nil {
		return Identity{}, err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return Identity{}, err
	}
	if err := directory.Close(); err != nil {
		return Identity{}, err
	}
	if err := os.Rename(stagingDirectory, store.directory); err != nil {
		return Identity{}, fmt.Errorf("activate node identity: %w", err)
	}
	return identity, nil
}

func generateCSR(hostname string) ([]byte, []byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate mTLS private key: %w", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname},
	}, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create mTLS CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}), nil
}

func (store *Store) Bootstrap(ctx context.Context, client *control.Client, token, version, hostname string) (Identity, error) {
	if strings.TrimSpace(token) == "" {
		return Identity{}, errors.New("bootstrap token is required when no identity is stored")
	}
	csr, privateKey, err := generateCSR(hostname)
	if err != nil {
		return Identity{}, err
	}
	response, err := client.Bootstrap(ctx, control.BootstrapRequest{
		AgentVersion:   version,
		BootstrapToken: token,
		CSRPEM:         string(csr),
		Hostname:       hostname,
	})
	if err != nil {
		return Identity{}, err
	}
	return store.persist(response, privateKey)
}

func (store *Store) SigningKeys() (map[string]ed25519.PublicKey, error) {
	var bundle []control.SigningTrustKey
	if err := decodeStrictJSON(store.trustBundlePath(), &bundle); err != nil {
		return nil, fmt.Errorf("load signing trust bundle: %w", err)
	}
	if err := validateTrustBundle(bundle); err != nil {
		return nil, err
	}
	keys := make(map[string]ed25519.PublicKey, len(bundle))
	for _, entry := range bundle {
		raw, _ := base64.StdEncoding.DecodeString(entry.PublicKey)
		keys[entry.KeyID] = ed25519.PublicKey(raw)
	}
	return keys, nil
}
