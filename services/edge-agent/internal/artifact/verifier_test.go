package artifact_test

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cdn-console/edge-agent/internal/artifact"
	"github.com/cdn-console/edge-agent/internal/control"
)

func tarBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{
			Mode:     0o644,
			Name:     name,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func signedArtifact(t *testing.T, fileName string, fileContent []byte, expectedFileContent []byte) ([]byte, control.ArtifactMetadata, map[string]ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fileSum := sha256.Sum256(expectedFileContent)
	manifest := control.ArtifactManifest{
		Files: map[string]string{
			fileName: "sha256:" + hex.EncodeToString(fileSum[:]),
		},
		Generation:      "10000000-0000-4000-8000-000000000002:1",
		MinAgentVersion: "0.1.0",
		SigningKeyID:    "test-key",
		SiteID:          "10000000-0000-4000-8000-000000000002",
		TenantID:        "10000000-0000-4000-8000-000000000001",
		Version:         1,
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	content := tarBytes(t, map[string][]byte{
		fileName:        fileContent,
		"manifest.json": manifestJSON,
	})
	archiveSum := sha256.Sum256(content)
	metadata := control.ArtifactMetadata{
		CreatedAt:    time.Now(),
		Digest:       "sha256:" + hex.EncodeToString(archiveSum[:]),
		Key:          "test.tar",
		KeyID:        "test-key",
		Manifest:     manifest,
		ManifestPath: "manifest.json",
		Signature:    base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, archiveSum[:])),
		SizeBytes:    int64(len(content)),
	}
	return content, metadata, map[string]ed25519.PublicKey{"test-key": publicKey}
}

func TestVerifyAndExtractValidSignatureAndHashes(t *testing.T) {
	content, metadata, keys := signedArtifact(t, "conf/nginx.conf", []byte("events {}\n"), []byte("events {}\n"))
	destination := t.TempDir()

	if err := artifact.VerifyAndExtract(content, metadata, keys, int64(len(content)), "0.1.0", destination); err != nil {
		t.Fatalf("VerifyAndExtract() error = %v", err)
	}
}

func TestVerifyAndExtractRejectsInvalidSignature(t *testing.T) {
	content, metadata, keys := signedArtifact(t, "conf/nginx.conf", []byte("events {}\n"), []byte("events {}\n"))
	metadata.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))

	err := artifact.VerifyAndExtract(content, metadata, keys, int64(len(content)), "0.1.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestVerifyAndExtractRejectsUnknownKeyRotationID(t *testing.T) {
	content, metadata, keys := signedArtifact(t, "conf/nginx.conf", []byte("events {}\n"), []byte("events {}\n"))
	metadata.KeyID = "rotated-key"

	err := artifact.VerifyAndExtract(content, metadata, keys, int64(len(content)), "0.1.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("expected key rotation ID error, got %v", err)
	}
}

func TestVerifyAndExtractRejectsFileHashMismatch(t *testing.T) {
	content, metadata, keys := signedArtifact(t, "conf/nginx.conf", []byte("tampered\n"), []byte("events {}\n"))

	err := artifact.VerifyAndExtract(content, metadata, keys, int64(len(content)), "0.1.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestVerifyAndExtractRejectsPathTraversal(t *testing.T) {
	content, metadata, keys := signedArtifact(t, "../escaped.conf", []byte("bad\n"), []byte("bad\n"))

	err := artifact.VerifyAndExtract(content, metadata, keys, int64(len(content)), "0.1.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}

func TestVerifyAndExtractMergesTwoSitesInEitherOrder(t *testing.T) {
	shared := []byte("events {}\n")
	first, firstMetadata, firstKeys := signedArtifact(t, "conf/nginx.conf", shared, shared)
	second, secondMetadata, secondKeys := signedArtifact(t, "conf/nginx.conf", shared, shared)
	for _, order := range []struct {
		name      string
		artifacts [][]byte
		metadata  []control.ArtifactMetadata
		keys      []map[string]ed25519.PublicKey
	}{
		{"forward", [][]byte{first, second}, []control.ArtifactMetadata{firstMetadata, secondMetadata}, []map[string]ed25519.PublicKey{firstKeys, secondKeys}},
		{"reverse", [][]byte{second, first}, []control.ArtifactMetadata{secondMetadata, firstMetadata}, []map[string]ed25519.PublicKey{secondKeys, firstKeys}},
	} {
		t.Run(order.name, func(t *testing.T) {
			destination := t.TempDir()
			for index := range order.artifacts {
				if err := artifact.VerifyAndExtract(order.artifacts[index], order.metadata[index], order.keys[index], int64(len(order.artifacts[index])), "0.1.0", destination); err != nil {
					t.Fatalf("merge artifact %d: %v", index, err)
				}
			}
			content, err := os.ReadFile(filepath.Join(destination, "conf", "nginx.conf"))
			if err != nil || !bytes.Equal(content, shared) {
				t.Fatalf("merged shared file = %q, %v", content, err)
			}
		})
	}
}

func TestVerifyAndExtractRejectsSharedPathConflict(t *testing.T) {
	first, firstMetadata, firstKeys := signedArtifact(t, "conf/nginx.conf", []byte("first\n"), []byte("first\n"))
	second, secondMetadata, secondKeys := signedArtifact(t, "conf/nginx.conf", []byte("second\n"), []byte("second\n"))
	destination := t.TempDir()
	if err := artifact.VerifyAndExtract(first, firstMetadata, firstKeys, int64(len(first)), "0.1.0", destination); err != nil {
		t.Fatal(err)
	}
	err := artifact.VerifyAndExtract(second, secondMetadata, secondKeys, int64(len(second)), "0.1.0", destination)
	if err == nil || !strings.Contains(err.Error(), "artifacts conflict at shared path") {
		t.Fatalf("error = %v", err)
	}
}
