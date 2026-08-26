package acquire

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxSourceEntries         = 200_000
	maxSourceFileSize  int64 = 1 << 30
	maxSourceExpanded  int64 = 8 << 30
	maxPackageEntries        = 2_000
	maxPackageExpanded int64 = 256 << 20
	maxPAXHeader       int64 = 1 << 20
)

func extractSourceTarGzip(archivePath, destination string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open source gzip stream: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	seen := map[string]struct{}{}
	root := ""
	var total int64
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read source tar: %w", err)
		}
		entries++
		if entries > maxSourceEntries {
			return fmt.Errorf("source archive exceeds %d entries", maxSourceEntries)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if err := discardPAXGlobalHeader(reader, header, &total, maxSourceExpanded, "source"); err != nil {
				return err
			}
			continue
		}
		name, err := canonicalTarName(header.Name)
		if err != nil {
			return fmt.Errorf("invalid source archive path %q: %w", header.Name, err)
		}
		if header.Typeflag == tar.TypeDir && header.Size != 0 {
			return fmt.Errorf("source archive directory %q has non-zero size %d", name, header.Size)
		}
		parts := strings.SplitN(name, "/", 2)
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root {
			return errors.New("source archive does not have a single root directory")
		}
		if len(parts) == 1 {
			if header.Typeflag != tar.TypeDir {
				return errors.New("source archive root is not a directory")
			}
			continue
		}
		relative := parts[1]
		if _, ok := seen[relative]; ok {
			return fmt.Errorf("source archive contains duplicate path %q", relative)
		}
		seen[relative] = struct{}{}
		target, err := secureJoin(destination, relative)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxSourceFileSize {
				return fmt.Errorf("source archive file %q has invalid size %d", relative, header.Size)
			}
			total += header.Size
			if total > maxSourceExpanded {
				return fmt.Errorf("source archive exceeds %d expanded bytes", maxSourceExpanded)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if header.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			written, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract %q after %d bytes: %w", relative, written, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("source archive contains unsupported entry type %d at %q", header.Typeflag, relative)
		}
	}
	if root == "" {
		return errors.New("source archive is empty")
	}
	return nil
}

func readPackageTarGzip(data []byte) (map[string][]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open package gzip stream: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	files := map[string][]byte{}
	var total int64
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read package tar: %w", err)
		}
		entries++
		if entries > maxPackageEntries {
			return nil, fmt.Errorf("package archive exceeds %d entries", maxPackageEntries)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if err := discardPAXGlobalHeader(reader, header, &total, maxPackageExpanded, "package"); err != nil {
				return nil, err
			}
			continue
		}
		name, err := canonicalTarName(header.Name)
		if err != nil {
			return nil, fmt.Errorf("invalid package archive path %q: %w", header.Name, err)
		}
		if header.Typeflag == tar.TypeDir && header.Size != 0 {
			return nil, fmt.Errorf("package archive directory %q has non-zero size %d", name, header.Size)
		}
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return nil, fmt.Errorf("package archive contains unsupported entry type %d at %q", header.Typeflag, name)
		}
		if header.Size < 0 || header.Size > maxPackageExpanded {
			return nil, fmt.Errorf("package archive file %q has invalid size %d", name, header.Size)
		}
		total += header.Size
		if total > maxPackageExpanded {
			return nil, fmt.Errorf("package archive exceeds %d expanded bytes", maxPackageExpanded)
		}
		if _, exists := files[name]; exists {
			return nil, fmt.Errorf("package archive contains duplicate path %q", name)
		}
		content := make([]byte, header.Size)
		if _, err := io.ReadFull(reader, content); err != nil {
			return nil, fmt.Errorf("extract package path %q: %w", name, err)
		}
		files[name] = content
	}
	return files, nil
}

func discardPAXGlobalHeader(reader *tar.Reader, header *tar.Header, total *int64, expandedLimit int64, archiveKind string) error {
	if header.Size < 0 || header.Size > maxPAXHeader {
		return fmt.Errorf("%s archive PAX global header has invalid size %d", archiveKind, header.Size)
	}
	*total += header.Size
	if *total > expandedLimit {
		return fmt.Errorf("%s archive exceeds %d expanded bytes", archiveKind, expandedLimit)
	}
	if _, err := io.CopyN(io.Discard, reader, header.Size); err != nil {
		return fmt.Errorf("read %s archive PAX global header: %w", archiveKind, err)
	}
	return nil
}

func canonicalTarName(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return ".", nil
	}
	if strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || path.IsAbs(name) {
		return "", errors.New("path must be a relative POSIX path")
	}
	clean := path.Clean(name)
	if clean != name || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path must be canonical and remain inside the archive")
	}
	return clean, nil
}

func secureJoin(root, relative string) (string, error) {
	canonical, err := canonicalTarName(relative)
	if err != nil || canonical == "." {
		return "", fmt.Errorf("invalid relative path %q", relative)
	}
	target := filepath.Join(root, filepath.FromSlash(canonical))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes extraction root", relative)
	}
	return target, nil
}
