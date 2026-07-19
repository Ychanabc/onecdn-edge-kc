package artifact

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cdn-console/edge-agent/internal/control"
)

const maxArchiveEntries = 10_000

var digestPattern = regexp.MustCompile(`^sha256:([a-f0-9]{64})$`)

type archiveEntry struct {
	content []byte
	isDir   bool
	name    string
}

func safeArchivePath(name string) bool {
	if name == "" || strings.Contains(name, `\`) || path.IsAbs(name) {
		return false
	}
	cleaned := path.Clean(name)
	return cleaned == name && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func readArchive(content []byte, maxBytes int64) (map[string]archiveEntry, error) {
	if int64(len(content)) > maxBytes {
		return nil, errors.New("artifact exceeds configured size limit")
	}
	reader := tar.NewReader(bytes.NewReader(content))
	entries := make(map[string]archiveEntry)
	for count := 0; ; count++ {
		if count >= maxArchiveEntries {
			return nil, errors.New("artifact contains too many entries")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}
		if !safeArchivePath(header.Name) {
			return nil, fmt.Errorf("artifact contains unsafe path: %q", header.Name)
		}
		if _, duplicate := entries[header.Name]; duplicate {
			return nil, fmt.Errorf("artifact contains duplicate path: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			entries[header.Name] = archiveEntry{isDir: true, name: header.Name}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxBytes {
				return nil, fmt.Errorf("artifact entry has invalid size: %s", header.Name)
			}
			file, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil {
				return nil, fmt.Errorf("read artifact entry %s: %w", header.Name, err)
			}
			if int64(len(file)) != header.Size {
				return nil, fmt.Errorf("artifact entry size mismatch: %s", header.Name)
			}
			entries[header.Name] = archiveEntry{content: file, name: header.Name}
		default:
			return nil, fmt.Errorf("artifact entry type is forbidden: %s", header.Name)
		}
	}
	return entries, nil
}

func parseManifest(content []byte) (control.ArtifactManifest, error) {
	var manifest control.ArtifactManifest
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return control.ArtifactManifest{}, fmt.Errorf("decode artifact manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return control.ArtifactManifest{}, errors.New("artifact manifest has trailing JSON")
	}
	if manifest.Version != 1 ||
		manifest.TenantID == "" ||
		manifest.SiteID == "" ||
		manifest.Generation == "" ||
		manifest.MinAgentVersion == "" ||
		manifest.SigningKeyID == "" ||
		len(manifest.Files) == 0 {
		return control.ArtifactManifest{}, errors.New("artifact manifest identity is incomplete")
	}
	return manifest, nil
}

func semverCore(version string) ([3]int, error) {
	var parsed [3]int
	segments := strings.FieldsFunc(version, func(character rune) bool {
		return character == '-' || character == '+'
	})
	if len(segments) == 0 {
		return parsed, fmt.Errorf("version %q is not semantic", version)
	}
	core := segments[0]
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsed, fmt.Errorf("version %q is not semantic", version)
	}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return parsed, fmt.Errorf("version %q is not semantic", version)
		}
		parsed[index] = value
	}
	return parsed, nil
}

func versionAtLeast(current, minimum string) error {
	currentVersion, err := semverCore(current)
	if err != nil {
		return err
	}
	minimumVersion, err := semverCore(minimum)
	if err != nil {
		return err
	}
	for index := range currentVersion {
		if currentVersion[index] > minimumVersion[index] {
			return nil
		}
		if currentVersion[index] < minimumVersion[index] {
			return fmt.Errorf("agent %s is older than required %s", current, minimum)
		}
	}
	return nil
}

func verify(content []byte, metadata control.ArtifactMetadata, keys map[string]ed25519.PublicKey, maxBytes int64, agentVersion string) (map[string]archiveEntry, error) {
	if metadata.SizeBytes != int64(len(content)) || metadata.SizeBytes > maxBytes {
		return nil, errors.New("artifact size does not match metadata")
	}
	match := digestPattern.FindStringSubmatch(metadata.Digest)
	if match == nil {
		return nil, errors.New("artifact digest format is invalid")
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != match[1] {
		return nil, errors.New("artifact SHA-256 digest mismatch")
	}
	publicKey, exists := keys[metadata.KeyID]
	if !exists || len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("artifact signing key ID is not trusted: %s", metadata.KeyID)
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(metadata.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, errors.New("artifact detached signature is invalid")
	}
	if !ed25519.Verify(publicKey, sum[:], signature) {
		return nil, errors.New("artifact detached signature verification failed")
	}
	if metadata.Manifest.SigningKeyID != metadata.KeyID {
		return nil, errors.New("artifact manifest signing key ID does not match metadata")
	}
	if err := versionAtLeast(agentVersion, metadata.Manifest.MinAgentVersion); err != nil {
		return nil, err
	}

	entries, err := readArchive(content, maxBytes)
	if err != nil {
		return nil, err
	}
	manifestEntry, exists := entries[metadata.ManifestPath]
	if !exists || manifestEntry.isDir {
		return nil, errors.New("artifact manifest path is missing")
	}
	manifest, err := parseManifest(manifestEntry.content)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(manifest, metadata.Manifest) {
		return nil, errors.New("embedded artifact manifest does not match signed metadata")
	}
	if manifest.SigningKeyID != metadata.KeyID {
		return nil, errors.New("embedded manifest key rotation ID mismatch")
	}

	seenFiles := make(map[string]struct{}, len(manifest.Files))
	for manifestPath, expectedDigest := range manifest.Files {
		if !safeArchivePath(manifestPath) || manifestPath == metadata.ManifestPath {
			return nil, fmt.Errorf("manifest contains unsafe file path: %s", manifestPath)
		}
		entry, exists := entries[manifestPath]
		if !exists || entry.isDir {
			return nil, fmt.Errorf("manifest file is missing from artifact: %s", manifestPath)
		}
		fileSum := sha256.Sum256(entry.content)
		if expectedDigest != "sha256:"+hex.EncodeToString(fileSum[:]) {
			return nil, fmt.Errorf("manifest hash mismatch for %s", manifestPath)
		}
		seenFiles[manifestPath] = struct{}{}
	}
	for name, entry := range entries {
		if entry.isDir || name == metadata.ManifestPath {
			continue
		}
		if _, listed := seenFiles[name]; !listed {
			return nil, fmt.Errorf("artifact contains unlisted file: %s", name)
		}
	}
	return entries, nil
}

func writeEntry(destination string, entry archiveEntry) error {
	target := filepath.Join(destination, filepath.FromSlash(entry.name))
	relative, err := filepath.Rel(destination, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact path escaped staging directory: %s", entry.name)
	}
	if entry.isDir {
		return os.MkdirAll(target, 0o750)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	handle, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(target)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, entry.content) {
			return fmt.Errorf("artifacts conflict at shared path: %s", entry.name)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := handle.Write(entry.content); err != nil {
		handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		handle.Close()
		return err
	}
	return handle.Close()
}

func VerifyAndExtract(content []byte, metadata control.ArtifactMetadata, keys map[string]ed25519.PublicKey, maxBytes int64, agentVersion, destination string) error {
	entries, err := verify(content, metadata, keys, maxBytes, agentVersion)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := entries[name]
		if err := writeEntry(destination, entry); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
	}
	return nil
}
