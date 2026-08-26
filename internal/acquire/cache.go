package acquire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const cacheSchema = 1

const maxCacheMetadata = 256 << 10

type cacheMetadata struct {
	Schema     int        `json:"schema"`
	Kind       string     `json:"kind"`
	Identity   string     `json:"identity"`
	File       string     `json:"file"`
	HostOS     string     `json:"host_os,omitempty"`
	HostArch   string     `json:"host_arch,omitempty"`
	Provenance Provenance `json:"provenance"`
	Signature  []byte     `json:"signature,omitempty"`
}

func cacheRoot(configured string) (string, error) {
	if configured != "" {
		return filepath.Abs(configured)
	}
	if runnerTemp := strings.TrimSpace(os.Getenv("RUNNER_TEMP")); runnerTemp != "" {
		root, err := filepath.Abs(runnerTemp)
		if err != nil {
			return "", fmt.Errorf("resolve RUNNER_TEMP: %w", err)
		}
		return filepath.Join(root, "sliver-bof-e2e-cache"), nil
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	return filepath.Join(root, "sliver-bof-e2e"), nil
}

func cacheEntry(root, kind, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(root, kind, hex.EncodeToString(digest[:]))
}

func restoreCache(entry, kind, identity, hostOS, hostArch string) (string, Provenance, []byte, bool) {
	metadataData, err := readCacheMetadata(filepath.Join(entry, "metadata.json"))
	if err != nil {
		return "", Provenance{}, nil, false
	}
	metadata := cacheMetadata{}
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return "", Provenance{}, nil, false
	}
	if metadata.Schema != cacheSchema || metadata.Kind != kind || metadata.Identity != identity ||
		metadata.HostOS != hostOS || metadata.HostArch != hostArch || metadata.File == "" || metadata.Provenance.Digest == "" {
		return "", Provenance{}, nil, false
	}
	if filepath.Base(metadata.File) != metadata.File {
		return "", Provenance{}, nil, false
	}
	artifact := filepath.Join(entry, metadata.File)
	info, err := os.Stat(artifact)
	if err != nil || !info.Mode().IsRegular() {
		return "", Provenance{}, nil, false
	}
	digest, err := sha256File(artifact)
	if err != nil || digest != metadata.Provenance.Digest {
		return "", Provenance{}, nil, false
	}
	metadata.Provenance.CacheHit = true
	return artifact, metadata.Provenance, append([]byte(nil), metadata.Signature...), true
}

func readCacheMetadata(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCacheMetadata {
		return nil, errors.New("cache metadata is not a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(file, maxCacheMetadata+1))
}

func storeCache(entry, kind, identity, hostOS, hostArch, sourcePath string, provenance Provenance, signature []byte) (string, Provenance, []byte, error) {
	if path, winner, winnerSignature, ok := restoreCache(entry, kind, identity, hostOS, hostArch); ok {
		return path, winner, winnerSignature, nil
	}
	if _, err := os.Lstat(entry); err == nil {
		return "", Provenance{}, nil, errors.New("refusing to replace an invalid cache entry")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", Provenance{}, nil, err
	}
	parent := filepath.Dir(entry)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", Provenance{}, nil, err
	}
	temporary, err := os.MkdirTemp(parent, ".cache-entry-*")
	if err != nil {
		return "", Provenance{}, nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(temporary)
		}
	}()
	fileName := "artifact"
	if hostOS == "windows" || filepath.Ext(sourcePath) == ".exe" {
		fileName += ".exe"
	}
	destination := filepath.Join(temporary, fileName)
	if err := copyRegularFile(sourcePath, destination, 0o700); err != nil {
		return "", Provenance{}, nil, err
	}
	metadata := cacheMetadata{
		Schema: cacheSchema, Kind: kind, Identity: identity, File: fileName,
		HostOS: hostOS, HostArch: hostArch, Provenance: provenance,
		Signature: append([]byte(nil), signature...),
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", Provenance{}, nil, err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(temporary, "metadata.json"), encoded, 0o600); err != nil {
		return "", Provenance{}, nil, err
	}
	if err := os.Rename(temporary, entry); err != nil {
		// Another process can win publication after the initial lookup. Its
		// complete content-addressed entry is authoritative; never delete it.
		if path, winner, winnerSignature, ok := restoreCache(entry, kind, identity, hostOS, hostArch); ok {
			return path, winner, winnerSignature, nil
		}
		return "", Provenance{}, nil, fmt.Errorf("publish cache entry: %w", err)
	}
	success = true
	return filepath.Join(entry, fileName), provenance, append([]byte(nil), signature...), nil
}

func copyRegularFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(destination, mode); err != nil {
			return err
		}
	}
	ok = true
	return nil
}
