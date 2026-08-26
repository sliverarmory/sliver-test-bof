package acquire

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	coffLoaderVersion      = "v1.0.16"
	coffLoaderPublicKey    = "RWS76vh9dCN1PXI/+xiuXPdDFAM+lI0+AT44AmrISFSbuT/EVsoRS+a0"
	coffLoaderArchiveURL   = "https://github.com/sliverarmory/COFFLoader/releases/download/v1.0.16/coff-loader.tar.gz"
	coffLoaderSignatureURL = "https://github.com/sliverarmory/COFFLoader/releases/download/v1.0.16/coff-loader.minisig"
	coffLoaderArchiveSHA   = "ccc4e879b1f60627765745743dc8d5660e679a670890b7dc4f54c075bb301eef"
	coffLoaderSignatureSHA = "6599f1984d692d0da6f27956d2caf669952edb7a9a48ff0417451e7f3ed0d6fc"
	coffLoaderAMD64SHA     = "bb25d81f9f69a944b62dad802a890f3d48e178fb4b6f882a1aae04e2a1be046a"
	coffLoader386SHA       = "86f192d1ad3f254a2d84be7d451ba5cdddf5d9bd09f01980321604a8ee18f848"
	coffLoaderRepository   = "https://github.com/sliverarmory/COFFLoader"
	coffLoaderExport       = "LoadAndRun"
	coffLoaderMaxFile      = 32 << 20
)

type loaderManifest struct {
	Name            string               `json:"name"`
	CommandName     string               `json:"command_name"`
	Version         string               `json:"version"`
	ExtensionAuthor string               `json:"extension_author"`
	OriginalAuthor  string               `json:"original_author"`
	RepoURL         string               `json:"repo_url"`
	Help            string               `json:"help"`
	Entrypoint      string               `json:"entrypoint"`
	Init            string               `json:"init,omitempty"`
	Files           []loaderManifestFile `json:"files"`
}

type loaderManifestFile struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Path string `json:"path"`
}

// COFFLoader returns the exact legacy loader for target. The built-in pinned
// package only supports windows/386 and windows/amd64.
func COFFLoader(ctx context.Context, target Target, options COFFLoaderOptions) (Loader, error) {
	if ctx == nil {
		return Loader{}, errors.New("nil context")
	}
	target.OS = strings.ToLower(strings.TrimSpace(target.OS))
	target.Arch = strings.ToLower(strings.TrimSpace(target.Arch))
	if target.OS != "windows" || target.Arch != "386" && target.Arch != "amd64" {
		return Loader{}, fmt.Errorf("legacy COFFLoader supports only windows/386 and windows/amd64, not %s/%s", target.OS, target.Arch)
	}
	if options.Path != "" {
		return localCOFFLoader(options)
	}
	if options.Init != "" || options.Export != "" {
		return Loader{}, errors.New("init/export overrides require an explicit COFFLoader path")
	}
	root, err := cacheRoot(options.CacheDir)
	if err != nil {
		return Loader{}, err
	}
	customURLs := options.ArchiveURL != "" || options.SignatureURL != ""
	identity := strings.Join([]string{"coffloader", coffLoaderVersion, coffLoaderArchiveSHA, target.OS, target.Arch}, "|")
	entry := cacheEntry(root, "coffloader", identity)
	if !customURLs {
		cachedPath, provenance, _, ok := restoreCache(entry, "coffloader", identity, target.OS, target.Arch)
		if ok {
			data, err := readRegularFileLimit(cachedPath, coffLoaderMaxFile)
			if err == nil && provenance.Digest == pinnedLoaderDigest(target.Arch) {
				provenance.Selector = coffLoaderVersion
				provenance.ResolvedTag = coffLoaderVersion
				provenance.ResolvedSHA = coffLoaderArchiveSHA
				provenance.Verified = true
				provenance.Source = coffLoaderArchiveURL
				logf(options.Logger, "using cached COFFLoader %s for %s/%s", coffLoaderVersion, target.OS, target.Arch)
				return Loader{Data: data, Export: coffLoaderExport, Digest: provenance.Digest, Provenance: provenance}, nil
			}
		}
	}
	archiveURL := options.ArchiveURL
	if archiveURL == "" {
		archiveURL = coffLoaderArchiveURL
	}
	signatureURL := options.SignatureURL
	if signatureURL == "" {
		signatureURL = coffLoaderSignatureURL
	}
	if (options.ArchiveURL == "") != (options.SignatureURL == "") {
		return Loader{}, errors.New("COFFLoader archive and signature URLs must be overridden together")
	}
	if options.ArchiveURL == "" {
		if err := requireOfficialCOFFLoaderURL(archiveURL); err != nil {
			return Loader{}, err
		}
		if err := requireOfficialCOFFLoaderURL(signatureURL); err != nil {
			return Loader{}, err
		}
	}
	client := httpClient(options.HTTPClient)
	downloadToken := options.Token
	if customURLs {
		// A GitHub token is only sent to the pinned official download endpoints;
		// caller-provided mirrors must arrange their own transport authentication.
		downloadToken = ""
	}
	logf(options.Logger, "downloading signed COFFLoader %s", coffLoaderVersion)
	archive, err := downloadBytes(ctx, client, archiveURL, downloadToken, maxPackageExpanded)
	if err != nil {
		return Loader{}, fmt.Errorf("download COFFLoader archive: %w", err)
	}
	signature, err := downloadBytes(ctx, client, signatureURL, downloadToken, maxSignature)
	if err != nil {
		return Loader{}, fmt.Errorf("download COFFLoader signature: %w", err)
	}
	if options.ArchiveURL == "" {
		if digest := sha256Bytes(archive); digest != coffLoaderArchiveSHA {
			return Loader{}, fmt.Errorf("COFFLoader archive SHA-256 %s, expected %s", digest, coffLoaderArchiveSHA)
		}
		if digest := sha256Bytes(signature); digest != coffLoaderSignatureSHA {
			return Loader{}, fmt.Errorf("COFFLoader signature SHA-256 %s, expected %s", digest, coffLoaderSignatureSHA)
		}
	}
	trustedComment, err := verifyMinisignBytes(coffLoaderPublicKey, archive, signature)
	if err != nil {
		return Loader{}, fmt.Errorf("verify COFFLoader package: %w", err)
	}
	signedManifest, err := base64.StdEncoding.DecodeString(trustedComment)
	if err != nil {
		return Loader{}, fmt.Errorf("decode signed COFFLoader manifest: %w", err)
	}
	manifest, err := parseLoaderManifest(signedManifest)
	if err != nil {
		return Loader{}, err
	}
	files, err := readPackageTarGzip(archive)
	if err != nil {
		return Loader{}, err
	}
	archivedManifest, ok := files["extension.json"]
	if !ok {
		return Loader{}, errors.New("COFFLoader archive does not contain extension.json")
	}
	if !bytes.Equal(archivedManifest, signedManifest) {
		return Loader{}, errors.New("COFFLoader archive manifest differs from the signed manifest")
	}
	declaredPath, err := exactLoaderPath(manifest, target)
	if err != nil {
		return Loader{}, err
	}
	data, ok := files[declaredPath]
	if !ok {
		return Loader{}, fmt.Errorf("COFFLoader archive does not contain exact manifest path %q", declaredPath)
	}
	if len(data) == 0 || len(data) > coffLoaderMaxFile {
		return Loader{}, fmt.Errorf("COFFLoader artifact size %d is outside limits", len(data))
	}
	digest := sha256Bytes(data)
	if !customURLs && digest != pinnedLoaderDigest(target.Arch) {
		return Loader{}, fmt.Errorf("COFFLoader %s artifact SHA-256 %s does not match the pinned package", target.Arch, digest)
	}
	provenance := Provenance{
		Selector: coffLoaderVersion, ResolvedTag: coffLoaderVersion, ResolvedSHA: coffLoaderArchiveSHA,
		Digest: digest, Verified: true, Source: archiveURL,
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Loader{}, err
	}
	temporary, err := os.CreateTemp(root, ".coffloader-*")
	if err != nil {
		return Loader{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return Loader{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Loader{}, err
	}
	if err := temporary.Close(); err != nil {
		return Loader{}, err
	}
	if !customURLs {
		if cachedPath, storedProvenance, _, err := storeCache(entry, "coffloader", identity, target.OS, target.Arch, temporaryPath, provenance, nil); err != nil {
			return Loader{}, fmt.Errorf("cache COFFLoader: %w", err)
		} else if storedProvenance.CacheHit {
			cachedData, err := readRegularFileLimit(cachedPath, coffLoaderMaxFile)
			if err != nil || storedProvenance.Digest != pinnedLoaderDigest(target.Arch) || sha256Bytes(cachedData) != storedProvenance.Digest {
				return Loader{}, errors.New("concurrently cached COFFLoader does not match the pinned artifact")
			}
			data = cachedData
			provenance = storedProvenance
			provenance.Selector = coffLoaderVersion
			provenance.ResolvedTag = coffLoaderVersion
			provenance.ResolvedSHA = coffLoaderArchiveSHA
			provenance.Verified = true
			provenance.Source = coffLoaderArchiveURL
		}
	}
	return Loader{
		Data: append([]byte(nil), data...), Init: manifest.Init, Export: manifest.Entrypoint,
		Digest: digest, Provenance: provenance,
	}, nil
}

func localCOFFLoader(options COFFLoaderOptions) (Loader, error) {
	filePath, err := filepath.Abs(options.Path)
	if err != nil {
		return Loader{}, err
	}
	data, err := readRegularFileLimit(filePath, coffLoaderMaxFile)
	if err != nil {
		return Loader{}, fmt.Errorf("read explicit COFFLoader: %w", err)
	}
	export := strings.TrimSpace(options.Export)
	if export == "" {
		export = coffLoaderExport
	}
	if strings.ContainsAny(export, " \t\r\n\x00") {
		return Loader{}, errors.New("COFFLoader export must be a single non-empty symbol")
	}
	init := strings.TrimSpace(options.Init)
	if strings.ContainsAny(init, "\r\n\x00") {
		return Loader{}, errors.New("COFFLoader init contains invalid characters")
	}
	digest := sha256Bytes(data)
	provenance := Provenance{Selector: "path", ResolvedSHA: digest, Digest: digest, Source: filePath}
	return Loader{Data: data, Init: init, Export: export, Digest: digest, Provenance: provenance}, nil
}

func parseLoaderManifest(data []byte) (loaderManifest, error) {
	manifest := loaderManifest{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return loaderManifest{}, fmt.Errorf("parse signed COFFLoader manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return loaderManifest{}, errors.New("signed COFFLoader manifest contains trailing data")
	}
	if manifest.CommandName != "coff-loader" || manifest.Version != coffLoaderVersion ||
		manifest.RepoURL != coffLoaderRepository || manifest.Entrypoint != coffLoaderExport {
		return loaderManifest{}, fmt.Errorf("signed COFFLoader manifest identity or routing does not match pinned %s package", coffLoaderVersion)
	}
	if manifest.Init != "" {
		return loaderManifest{}, fmt.Errorf("signed COFFLoader manifest has unexpected init %q", manifest.Init)
	}
	return manifest, nil
}

func exactLoaderPath(manifest loaderManifest, target Target) (string, error) {
	match := ""
	for _, file := range manifest.Files {
		if strings.ToLower(file.OS) != target.OS || strings.ToLower(file.Arch) != target.Arch {
			continue
		}
		if match != "" {
			return "", fmt.Errorf("COFFLoader manifest contains duplicate %s/%s artifacts", target.OS, target.Arch)
		}
		canonical, err := canonicalTarName(file.Path)
		if err != nil || canonical == "." || path.IsAbs(canonical) {
			return "", fmt.Errorf("COFFLoader manifest path %q is invalid", file.Path)
		}
		match = canonical
	}
	if match == "" {
		return "", fmt.Errorf("COFFLoader manifest has no exact %s/%s artifact", target.OS, target.Arch)
	}
	return match, nil
}

func pinnedLoaderDigest(arch string) string {
	if arch == "386" {
		return coffLoader386SHA
	}
	if arch == "amd64" {
		return coffLoaderAMD64SHA
	}
	return ""
}

func readRegularFileLimit(filePath string, limit int64) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("file size %d is outside limits", info.Size())
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, err
	}
	return data, nil
}

func requireOfficialCOFFLoaderURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("official COFFLoader URL must use https://github.com without extra URL components")
	}
	return nil
}
