package acquire

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	defaultGitHubAPI = "https://api.github.com"
	sliverPublicKey  = "RWTZPg959v3b7tLG7VzKHRB1/QT+d3c71Uzetfa44qAoX5rH7mGoQTTR"

	maxGitHubJSON    = 8 << 20
	maxServerAsset   = 768 << 20
	maxSignature     = 64 << 10
	maxSourceArchive = 2 << 30
	sourceBuildABI   = "source-build-v1:go_sqlite,server:trimpath:vendor"
)

var (
	releaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	fullSHA           = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	fullLowerSHA      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	goVersionPattern  = regexp.MustCompile(`^1\.([0-9]+)(?:\.([0-9]+))?$`)
	toolchainPattern  = regexp.MustCompile(`^go1\.[0-9]+(?:\.[0-9]+)?$`)
)

// Resolve converts latest/master into a selector suitable for every job in a
// matrix. Release selectors resolve to an exact tag and its backing commit;
// source selectors resolve to a full commit SHA. It does not download assets or
// build Sliver.
func Resolve(ctx context.Context, selector, token string) (Resolution, error) {
	return resolveSelector(ctx, selector, token, httpClient(nil), defaultGitHubAPI)
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubCommit struct {
	SHA string `json:"sha"`
}

type githubGitObject struct {
	Object *struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object,omitempty"`
}

type githubContents struct {
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// Server returns an executable Sliver server and verified provenance.
func Server(ctx context.Context, options Options) (string, Provenance, error) {
	if ctx == nil {
		return "", Provenance{}, errors.New("nil context")
	}
	hostOS, hostArch, err := normalizeHost(options.HostOS, options.HostArch)
	if err != nil {
		return "", Provenance{}, err
	}
	if options.ServerPath != "" {
		return localServer(options.ServerPath)
	}
	selector := strings.TrimSpace(options.Version)
	if selector == "" {
		selector = "master"
	}
	client := httpClient(options.HTTPClient)
	apiBase := strings.TrimRight(options.GitHubAPIURL, "/")
	if apiBase == "" {
		apiBase = defaultGitHubAPI
	}
	if selector == "latest" || releaseTagPattern.MatchString(selector) || isPinnedReleaseSelector(selector) {
		return acquireReleaseServer(ctx, options, client, apiBase, selector, hostOS, hostArch)
	}
	if selector == "master" || fullSHA.MatchString(selector) {
		return acquireSourceServer(ctx, options, client, apiBase, selector, hostOS, hostArch)
	}
	return "", Provenance{}, fmt.Errorf("unsupported Sliver version %q: use latest, a v-prefixed release tag, a pinned release selector, master, or a full commit SHA", selector)
}

func resolveSelector(ctx context.Context, selector, token string, client *http.Client, apiBase string) (Resolution, error) {
	if ctx == nil {
		return Resolution{}, errors.New("nil context")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		selector = "master"
	}
	if selector == "latest" || releaseTagPattern.MatchString(selector) || isPinnedReleaseSelector(selector) {
		releaseTag, expectedSHA, pinned := parsePinnedReleaseSelector(selector)
		requestedTag := selector
		if pinned {
			requestedTag = releaseTag
		}
		endpoint := apiPath(apiBase, "repos", "BishopFox", "sliver", "releases", "tags", requestedTag)
		if selector == "latest" {
			endpoint = apiPath(apiBase, "repos", "BishopFox", "sliver", "releases", "latest")
		}
		release := githubRelease{}
		if err := githubJSON(ctx, client, endpoint, token, &release); err != nil {
			return Resolution{}, fmt.Errorf("resolve Sliver release %q: %w", selector, err)
		}
		if !releaseTagPattern.MatchString(release.TagName) {
			return Resolution{}, fmt.Errorf("GitHub returned invalid Sliver release tag %q", release.TagName)
		}
		if selector != "latest" && release.TagName != requestedTag {
			return Resolution{}, fmt.Errorf("GitHub release tag %q does not match requested %q", release.TagName, requestedTag)
		}
		sha, err := resolveTagCommit(ctx, client, apiBase, token, release.TagName)
		if err != nil {
			return Resolution{}, fmt.Errorf("resolve Sliver tag %s: %w", release.TagName, err)
		}
		if pinned && sha != expectedSHA {
			return Resolution{}, fmt.Errorf("Sliver release tag %s now resolves to %s, expected pinned commit %s", release.TagName, sha, expectedSHA)
		}
		return Resolution{
			Requested: selector, Selector: pinnedReleaseSelector(release.TagName, sha), ResolvedTag: release.TagName,
			ResolvedSHA: sha,
		}, nil
	}
	if selector == "master" || fullSHA.MatchString(selector) {
		commit := githubCommit{}
		if err := githubJSON(ctx, client, apiPath(apiBase, "repos", "BishopFox", "sliver", "commits", selector), token, &commit); err != nil {
			return Resolution{}, fmt.Errorf("resolve Sliver commit %q: %w", selector, err)
		}
		commit.SHA = strings.ToLower(commit.SHA)
		if !fullSHA.MatchString(commit.SHA) {
			return Resolution{}, fmt.Errorf("GitHub returned invalid Sliver commit SHA %q", commit.SHA)
		}
		if fullSHA.MatchString(selector) && !strings.EqualFold(selector, commit.SHA) {
			return Resolution{}, fmt.Errorf("resolved commit %s does not match requested SHA %s", commit.SHA, selector)
		}
		return Resolution{
			Requested: selector, Selector: commit.SHA, ResolvedSHA: commit.SHA, SourceBuilt: true,
		}, nil
	}
	return Resolution{}, fmt.Errorf("unsupported Sliver version %q: use latest, a v-prefixed release tag, a pinned release selector, master, or a full commit SHA", selector)
}

func pinnedReleaseSelector(tag, sha string) string {
	return "release:" + tag + "@" + strings.ToLower(sha)
}

func parsePinnedReleaseSelector(selector string) (tag, sha string, ok bool) {
	remainder, found := strings.CutPrefix(selector, "release:")
	if !found {
		return "", "", false
	}
	tag, sha, found = strings.Cut(remainder, "@")
	if !found || !releaseTagPattern.MatchString(tag) || !fullLowerSHA.MatchString(sha) {
		return "", "", false
	}
	return tag, sha, true
}

func isPinnedReleaseSelector(selector string) bool {
	_, _, ok := parsePinnedReleaseSelector(selector)
	return ok
}

func normalizeHost(hostOS, hostArch string) (string, string, error) {
	if hostOS == "" {
		hostOS = runtime.GOOS
	}
	if hostArch == "" {
		hostArch = runtime.GOARCH
	}
	hostOS = strings.ToLower(strings.TrimSpace(hostOS))
	hostArch = strings.ToLower(strings.TrimSpace(hostArch))
	if hostOS == "macos" {
		hostOS = "darwin"
	}
	if hostOS != "linux" && hostOS != "darwin" && hostOS != "windows" {
		return "", "", fmt.Errorf("unsupported server host OS %q", hostOS)
	}
	if hostArch != "amd64" && hostArch != "arm64" {
		return "", "", fmt.Errorf("unsupported server host architecture %q: Sliver does not publish 386 server binaries", hostArch)
	}
	return hostOS, hostArch, nil
}

func localServer(configuredPath string) (string, Provenance, error) {
	path, err := filepath.Abs(configuredPath)
	if err != nil {
		return "", Provenance{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("stat local Sliver server: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", Provenance{}, errors.New("local Sliver server is not a regular file")
	}
	digest, err := sha256File(path)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("hash local Sliver server: %w", err)
	}
	return path, Provenance{Selector: "path", ResolvedSHA: digest, Digest: digest, Source: path}, nil
}

func acquireReleaseServer(ctx context.Context, options Options, client *http.Client, apiBase, selector, hostOS, hostArch string) (string, Provenance, error) {
	releaseTag, expectedSHA, pinned := parsePinnedReleaseSelector(selector)
	requestedTag := selector
	if pinned {
		requestedTag = releaseTag
	}
	releaseEndpoint := apiPath(apiBase, "repos", "BishopFox", "sliver", "releases", "tags", requestedTag)
	if selector == "latest" {
		releaseEndpoint = apiPath(apiBase, "repos", "BishopFox", "sliver", "releases", "latest")
	}
	release := githubRelease{}
	if err := githubJSON(ctx, client, releaseEndpoint, options.Token, &release); err != nil {
		return "", Provenance{}, fmt.Errorf("resolve Sliver release %q: %w", selector, err)
	}
	if !releaseTagPattern.MatchString(release.TagName) {
		return "", Provenance{}, fmt.Errorf("GitHub returned invalid Sliver release tag %q", release.TagName)
	}
	if selector != "latest" && release.TagName != requestedTag {
		return "", Provenance{}, fmt.Errorf("GitHub release tag %q does not match requested %q", release.TagName, requestedTag)
	}
	commitSHA, err := resolveTagCommit(ctx, client, apiBase, options.Token, release.TagName)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("resolve Sliver tag %s: %w", release.TagName, err)
	}
	if pinned && commitSHA != expectedSHA {
		return "", Provenance{}, fmt.Errorf("Sliver release tag %s now resolves to %s, expected pinned commit %s", release.TagName, commitSHA, expectedSHA)
	}
	resolvedSelector := pinnedReleaseSelector(release.TagName, commitSHA)
	assetName := "sliver-server_" + hostOS + "-" + hostArch
	if hostOS == "windows" {
		assetName += ".exe"
	}
	asset, signatureAsset, err := exactReleaseAssets(release.Assets, assetName)
	if err != nil {
		return "", Provenance{}, err
	}
	if err := validateGitHubDownloadURL(asset.BrowserDownloadURL, apiBase); err != nil {
		return "", Provenance{}, fmt.Errorf("invalid release asset URL: %w", err)
	}
	if err := validateGitHubDownloadURL(signatureAsset.BrowserDownloadURL, apiBase); err != nil {
		return "", Provenance{}, fmt.Errorf("invalid release signature URL: %w", err)
	}
	assetDigest, err := githubAssetSHA256(asset)
	if err != nil {
		return "", Provenance{}, err
	}
	signatureDigest, err := githubAssetSHA256(signatureAsset)
	if err != nil {
		return "", Provenance{}, err
	}
	identity := releaseCacheIdentity(release.TagName, commitSHA, hostOS, hostArch, asset, signatureAsset, assetDigest, signatureDigest)
	root, err := cacheRoot(options.CacheDir)
	if err != nil {
		return "", Provenance{}, err
	}
	entry := cacheEntry(root, "server", identity)
	if path, provenance, cachedSignature, ok := restoreCache(entry, "server", identity, hostOS, hostArch); ok {
		if err := verifyReleaseDigests(provenance.Digest, cachedSignature, assetDigest, signatureDigest); err == nil {
			if err := verifyCachedServer(path, cachedSignature, assetName); err == nil {
				provenance.Selector = resolvedSelector
				provenance.ResolvedTag = release.TagName
				provenance.ResolvedSHA = commitSHA
				provenance.SourceBuilt = false
				provenance.Verified = true
				provenance.Source = asset.BrowserDownloadURL
				logf(options.Logger, "using cached Sliver server %s (%s)", release.TagName, provenance.Digest)
				return path, provenance, nil
			}
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", Provenance{}, err
	}
	logf(options.Logger, "downloading Sliver server %s for %s/%s", release.TagName, hostOS, hostArch)
	temporaryPath, digest, err := downloadFile(ctx, client, asset.BrowserDownloadURL, options.Token, root, asset.Size, maxServerAsset)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("download Sliver server asset: %w", err)
	}
	defer os.Remove(temporaryPath)
	if assetDigest != "" && digest != assetDigest {
		return "", Provenance{}, fmt.Errorf("Sliver server asset SHA-256 %s, expected GitHub digest %s", digest, assetDigest)
	}
	signature, err := downloadBytes(ctx, client, signatureAsset.BrowserDownloadURL, options.Token, maxSignature)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("download Sliver server signature: %w", err)
	}
	if int64(len(signature)) != signatureAsset.Size {
		return "", Provenance{}, fmt.Errorf("Sliver server signature is %d bytes, expected %d", len(signature), signatureAsset.Size)
	}
	if signatureDigest != "" {
		actualSignatureDigest := sha256Bytes(signature)
		if actualSignatureDigest != signatureDigest {
			return "", Provenance{}, fmt.Errorf("Sliver server signature SHA-256 %s, expected GitHub digest %s", actualSignatureDigest, signatureDigest)
		}
	}
	file, err := os.Open(temporaryPath)
	if err != nil {
		return "", Provenance{}, err
	}
	trustedComment, verifyErr := verifyMinisignReader(sliverPublicKey, file, signature)
	closeErr := file.Close()
	if verifyErr != nil {
		return "", Provenance{}, fmt.Errorf("verify Sliver server signature: %w", verifyErr)
	}
	if closeErr != nil {
		return "", Provenance{}, closeErr
	}
	if signedFile := trustedCommentFile(trustedComment); signedFile != assetName {
		return "", Provenance{}, fmt.Errorf("Sliver signature file field %q, expected %q", signedFile, assetName)
	}
	provenance := Provenance{
		Selector: resolvedSelector, ResolvedTag: release.TagName, ResolvedSHA: commitSHA,
		Digest: digest, Verified: true, Source: asset.BrowserDownloadURL,
	}
	path, storedProvenance, storedSignature, err := storeCache(entry, "server", identity, hostOS, hostArch, temporaryPath, provenance, signature)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("cache Sliver server: %w", err)
	}
	if storedProvenance.CacheHit {
		if err := verifyReleaseDigests(storedProvenance.Digest, storedSignature, assetDigest, signatureDigest); err != nil {
			return "", Provenance{}, fmt.Errorf("verify concurrently cached Sliver server digest: %w", err)
		}
		if err := verifyCachedServer(path, storedSignature, assetName); err != nil {
			return "", Provenance{}, fmt.Errorf("verify concurrently cached Sliver server signature: %w", err)
		}
		storedProvenance.Selector = resolvedSelector
		storedProvenance.ResolvedTag = release.TagName
		storedProvenance.ResolvedSHA = commitSHA
		storedProvenance.SourceBuilt = false
		storedProvenance.Verified = true
		storedProvenance.Source = asset.BrowserDownloadURL
		return path, storedProvenance, nil
	}
	return path, provenance, nil
}

func releaseCacheIdentity(tag, commitSHA, hostOS, hostArch string, asset, signature githubAsset, assetDigest, signatureDigest string) string {
	return strings.Join([]string{
		"release", tag, commitSHA, hostOS, hostArch, asset.Name,
		strconv.FormatInt(asset.ID, 10), assetDigest,
		strconv.FormatInt(signature.ID, 10), signatureDigest,
	}, "|")
}

func githubAssetSHA256(asset githubAsset) (string, error) {
	if asset.Digest == "" {
		return "", nil
	}
	algorithm, digest, found := strings.Cut(asset.Digest, ":")
	if !found || !strings.EqualFold(algorithm, "sha256") || !sha256Pattern.MatchString(digest) {
		return "", fmt.Errorf("release asset %q has unsupported GitHub digest %q", asset.Name, asset.Digest)
	}
	return strings.ToLower(digest), nil
}

func verifyReleaseDigests(artifactDigest string, signature []byte, expectedArtifact, expectedSignature string) error {
	if expectedArtifact != "" && artifactDigest != expectedArtifact {
		return fmt.Errorf("artifact SHA-256 %s does not match GitHub digest %s", artifactDigest, expectedArtifact)
	}
	if expectedSignature != "" {
		actual := sha256Bytes(signature)
		if actual != expectedSignature {
			return fmt.Errorf("signature SHA-256 %s does not match GitHub digest %s", actual, expectedSignature)
		}
	}
	return nil
}

func acquireSourceServer(ctx context.Context, options Options, client *http.Client, apiBase, selector, hostOS, hostArch string) (string, Provenance, error) {
	commit := githubCommit{}
	if err := githubJSON(ctx, client, apiPath(apiBase, "repos", "BishopFox", "sliver", "commits", selector), options.Token, &commit); err != nil {
		return "", Provenance{}, fmt.Errorf("resolve Sliver commit %q: %w", selector, err)
	}
	commit.SHA = strings.ToLower(commit.SHA)
	if !fullSHA.MatchString(commit.SHA) {
		return "", Provenance{}, fmt.Errorf("GitHub returned invalid Sliver commit SHA %q", commit.SHA)
	}
	if fullSHA.MatchString(selector) && !strings.EqualFold(selector, commit.SHA) {
		return "", Provenance{}, fmt.Errorf("resolved commit %s does not match requested SHA %s", commit.SHA, selector)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		return "", Provenance{}, errors.New("Go toolchain is required to build Sliver master")
	}
	goMod, err := fetchGoMod(ctx, client, apiBase, options.Token, commit.SHA)
	if err != nil {
		return "", Provenance{}, err
	}
	if err := validateSliverGoMod(goMod); err != nil {
		return "", Provenance{}, err
	}
	requiredToolchain, err := requiredGoToolchain(goMod)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("select Sliver Go toolchain: %w", err)
	}
	toolchain, err := discoverToolchain(ctx, goPath, goMod, requiredToolchain)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("discover Sliver Go toolchain: %w", err)
	}
	identity := strings.Join([]string{"source", commit.SHA, hostOS, hostArch, requiredToolchain, toolchain, sourceBuildABI}, "|")
	root, err := cacheRoot(options.CacheDir)
	if err != nil {
		return "", Provenance{}, err
	}
	entry := cacheEntry(root, "server", identity)
	if path, provenance, _, ok := restoreCache(entry, "server", identity, hostOS, hostArch); ok {
		provenance.Selector = commit.SHA
		provenance.ResolvedTag = ""
		provenance.ResolvedSHA = commit.SHA
		provenance.SourceBuilt = true
		provenance.Verified = false
		provenance.Source = apiPath(apiBase, "repos", "BishopFox", "sliver", "tarball", commit.SHA)
		provenance.Toolchain = toolchain
		logf(options.Logger, "using cached source-built Sliver server %s (%s)", commit.SHA, provenance.Digest)
		return path, provenance, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", Provenance{}, err
	}
	work, err := os.MkdirTemp(root, ".sliver-source-build-*")
	if err != nil {
		return "", Provenance{}, err
	}
	defer os.RemoveAll(work)
	archiveURL := apiPath(apiBase, "repos", "BishopFox", "sliver", "tarball", commit.SHA)
	logf(options.Logger, "downloading Sliver source at %s", commit.SHA)
	archive, _, err := downloadFile(ctx, client, archiveURL, options.Token, work, 0, maxSourceArchive)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("download Sliver source: %w", err)
	}
	source := filepath.Join(work, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		return "", Provenance{}, err
	}
	if err := extractSourceTarGzip(archive, source); err != nil {
		return "", Provenance{}, fmt.Errorf("extract Sliver source: %w", err)
	}
	extractedGoMod, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		return "", Provenance{}, fmt.Errorf("read extracted Sliver go.mod: %w", err)
	}
	if !bytes.Equal(extractedGoMod, goMod) {
		return "", Provenance{}, errors.New("Sliver source go.mod differs from the resolved commit metadata")
	}
	logf(options.Logger, "preparing Sliver embedded build assets with %s", toolchain)
	if err := runLogged(ctx, options.Logger, source, commandEnv(false, "", "", requiredToolchain), goPath, "run", "-mod=vendor", "./util/cmd/assets", "--quiet", "--no-colors"); err != nil {
		return "", Provenance{}, fmt.Errorf("prepare Sliver build assets: %w", err)
	}
	binaryName := "sliver-server"
	if hostOS == "windows" {
		binaryName += ".exe"
	}
	output := filepath.Join(work, binaryName)
	logf(options.Logger, "building Sliver server for %s/%s", hostOS, hostArch)
	if err := runLogged(ctx, options.Logger, source, commandEnv(true, hostOS, hostArch, requiredToolchain), goPath,
		"build", "-buildvcs=false", "-mod=vendor", "-trimpath", "-tags", "go_sqlite,server", "-o", output, "./server"); err != nil {
		return "", Provenance{}, fmt.Errorf("build Sliver server: %w", err)
	}
	digest, err := sha256File(output)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("hash source-built Sliver server: %w", err)
	}
	provenance := Provenance{
		Selector: commit.SHA, ResolvedSHA: commit.SHA, Digest: digest, SourceBuilt: true,
		Source: archiveURL, Toolchain: toolchain,
	}
	path, storedProvenance, _, err := storeCache(entry, "server", identity, hostOS, hostArch, output, provenance, nil)
	if err != nil {
		return "", Provenance{}, fmt.Errorf("cache source-built Sliver server: %w", err)
	}
	if storedProvenance.CacheHit {
		storedProvenance.Selector = commit.SHA
		storedProvenance.ResolvedTag = ""
		storedProvenance.ResolvedSHA = commit.SHA
		storedProvenance.SourceBuilt = true
		storedProvenance.Verified = false
		storedProvenance.Source = archiveURL
		storedProvenance.Toolchain = toolchain
		return path, storedProvenance, nil
	}
	return path, provenance, nil
}

func exactReleaseAssets(assets []githubAsset, name string) (githubAsset, githubAsset, error) {
	var artifact, signature *githubAsset
	for index := range assets {
		asset := &assets[index]
		switch asset.Name {
		case name:
			if artifact != nil {
				return githubAsset{}, githubAsset{}, fmt.Errorf("release contains duplicate asset %q", name)
			}
			artifact = asset
		case name + ".minisig":
			if signature != nil {
				return githubAsset{}, githubAsset{}, fmt.Errorf("release contains duplicate asset %q", name+".minisig")
			}
			signature = asset
		}
	}
	if artifact == nil || signature == nil {
		return githubAsset{}, githubAsset{}, fmt.Errorf("release does not contain exact signed server asset %q", name)
	}
	if artifact.ID <= 0 || signature.ID <= 0 {
		return githubAsset{}, githubAsset{}, errors.New("release asset ID is missing or invalid")
	}
	if artifact.Size <= 0 || artifact.Size > maxServerAsset || signature.Size <= 0 || signature.Size > maxSignature {
		return githubAsset{}, githubAsset{}, errors.New("release asset size is missing or outside acquisition limits")
	}
	if _, err := githubAssetSHA256(*artifact); err != nil {
		return githubAsset{}, githubAsset{}, err
	}
	if _, err := githubAssetSHA256(*signature); err != nil {
		return githubAsset{}, githubAsset{}, err
	}
	return *artifact, *signature, nil
}

func verifyCachedServer(path string, signature []byte, expectedFile string) error {
	return verifyCachedServerWithKey(path, signature, expectedFile, sliverPublicKey)
}

func verifyCachedServerWithKey(path string, signature []byte, expectedFile, publicKey string) error {
	if len(signature) == 0 {
		return errors.New("cached release server has no signature")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	trustedComment, verifyErr := verifyMinisignReader(publicKey, file, signature)
	closeErr := file.Close()
	if verifyErr != nil {
		return verifyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if signedFile := trustedCommentFile(trustedComment); signedFile != expectedFile {
		return fmt.Errorf("cached Sliver signature file field %q, expected %q", signedFile, expectedFile)
	}
	return nil
}

func resolveTagCommit(ctx context.Context, client *http.Client, apiBase, token, tag string) (string, error) {
	reference := githubGitObject{}
	if err := githubJSON(ctx, client, apiPath(apiBase, "repos", "BishopFox", "sliver", "git", "ref", "tags", tag), token, &reference); err != nil {
		return "", err
	}
	if reference.Object == nil {
		return "", errors.New("Git tag reference response is missing its target object")
	}
	sha, objectType := reference.Object.SHA, reference.Object.Type
	for depth := 0; depth < 5; depth++ {
		if objectType == "commit" {
			sha = strings.ToLower(sha)
			if !fullSHA.MatchString(sha) {
				return "", fmt.Errorf("invalid tag commit SHA %q", sha)
			}
			return sha, nil
		}
		if objectType != "tag" || !fullSHA.MatchString(sha) {
			return "", fmt.Errorf("unsupported Git tag object type %q", objectType)
		}
		tagObject := githubGitObject{}
		if err := githubJSON(ctx, client, apiPath(apiBase, "repos", "BishopFox", "sliver", "git", "tags", sha), token, &tagObject); err != nil {
			return "", err
		}
		if tagObject.Object == nil {
			return "", errors.New("annotated tag response is missing its target object")
		}
		sha, objectType = tagObject.Object.SHA, tagObject.Object.Type
	}
	return "", errors.New("Git tag indirection exceeds five objects")
}

func fetchGoMod(ctx context.Context, client *http.Client, apiBase, token, sha string) ([]byte, error) {
	contents := githubContents{}
	endpoint := apiPath(apiBase, "repos", "BishopFox", "sliver", "contents", "go.mod") + "?ref=" + url.QueryEscape(sha)
	if err := githubJSON(ctx, client, endpoint, token, &contents); err != nil {
		return nil, fmt.Errorf("fetch Sliver go.mod: %w", err)
	}
	if contents.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected GitHub contents encoding %q", contents.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.Map(func(char rune) rune {
		if char == '\n' || char == '\r' {
			return -1
		}
		return char
	}, contents.Content))
	if err != nil {
		return nil, fmt.Errorf("decode Sliver go.mod: %w", err)
	}
	if len(decoded) == 0 || len(decoded) > 1<<20 {
		return nil, errors.New("Sliver go.mod is empty or exceeds 1 MiB")
	}
	return decoded, nil
}

func validateSliverGoMod(data []byte) error {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			if fields[1] != "github.com/bishopfox/sliver" {
				return fmt.Errorf("resolved source has unexpected module %q", fields[1])
			}
			return nil
		}
	}
	return errors.New("resolved source go.mod has no module directive")
}

func requiredGoToolchain(goMod []byte) (string, error) {
	goVersion := ""
	toolchain := ""
	for _, line := range strings.Split(string(goMod), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "go":
			if goVersion != "" {
				return "", errors.New("go.mod contains duplicate go directives")
			}
			goVersion = fields[1]
		case "toolchain":
			if toolchain != "" {
				return "", errors.New("go.mod contains duplicate toolchain directives")
			}
			toolchain = fields[1]
		}
	}
	if goVersion == "" {
		return "", errors.New("go.mod has no go directive")
	}
	match := goVersionPattern.FindStringSubmatch(goVersion)
	if match == nil {
		return "", fmt.Errorf("unsupported go directive %q", goVersion)
	}
	baseline := "go" + goVersion
	// Go 1.21 and newer toolchain downloads include the .0 patch component,
	// even when an older go.mod spells only the language version.
	if match[2] == "" {
		minor := 0
		if _, err := fmt.Sscanf(match[1], "%d", &minor); err != nil {
			return "", fmt.Errorf("parse go directive %q: %w", goVersion, err)
		}
		if minor >= 21 {
			baseline += ".0"
		}
	}
	if toolchain == "" || toolchain == "default" {
		return baseline, nil
	}
	if !toolchainPattern.MatchString(toolchain) {
		return "", fmt.Errorf("unsupported toolchain directive %q", toolchain)
	}
	return toolchain, nil
}

func discoverToolchain(ctx context.Context, goPath string, goMod []byte, requiredToolchain string) (string, error) {
	directory, err := os.MkdirTemp("", "sliver-go-toolchain-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), goMod, 0o600); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, goPath, "env", "GOVERSION")
	command.Dir = directory
	command.Env = commandEnv(false, "", "", requiredToolchain)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(output))
	if !strings.HasPrefix(version, "go1.") || strings.ContainsAny(version, " /\\") {
		return "", fmt.Errorf("unexpected Go toolchain version %q", version)
	}
	if version != requiredToolchain {
		return "", fmt.Errorf("Go selected %s, required exact Sliver toolchain %s", version, requiredToolchain)
	}
	return version, nil
}

func commandEnv(build bool, goos, goarch, requiredToolchain string) []string {
	return commandEnvFrom(os.Environ(), build, goos, goarch, requiredToolchain)
}

func commandEnvFrom(hostEnvironment []string, build bool, goos, goarch, requiredToolchain string) []string {
	allowed := []string{
		"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC",
		"TMP", "TEMP", "TMPDIR", "LANG", "LC_ALL", "TZ",
		"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"GOPROXY", "GONOPROXY", "GONOSUMDB", "GOSUMDB",
	}
	host := make(map[string]string, len(allowed))
	for _, item := range hostEnvironment {
		name, value, found := strings.Cut(item, "=")
		if found {
			host[strings.ToUpper(name)] = value
		}
	}
	environment := make([]string, 0, len(allowed)+16)
	for _, name := range allowed {
		if value, ok := host[name]; ok {
			environment = append(environment, name+"="+value)
		}
	}
	homeRoot := strings.TrimSpace(host["RUNNER_TEMP"])
	if homeRoot == "" {
		homeRoot = strings.TrimSpace(host["TMPDIR"])
	}
	if homeRoot == "" {
		homeRoot = strings.TrimSpace(host["TEMP"])
	}
	if homeRoot == "" {
		homeRoot = strings.TrimSpace(host["TMP"])
	}
	if homeRoot == "" {
		homeRoot = os.TempDir()
	}
	if absoluteRoot, err := filepath.Abs(homeRoot); err == nil {
		homeRoot = absoluteRoot
	} else {
		homeRoot = os.TempDir()
	}
	isolatedHome := filepath.Join(homeRoot, "sliver-bof-e2e-go-home")
	goPath := filepath.Join(isolatedHome, "go")
	environment = append(environment,
		"HOME="+isolatedHome,
		"USERPROFILE="+isolatedHome,
		"XDG_CONFIG_HOME="+filepath.Join(isolatedHome, ".config"),
		// Never depend on Windows LocalAppData or caller/self-hosted Go
		// caches. These canonical action-private paths also prevent
		// case-variant environment keys from selecting a different cache.
		"GOCACHE="+filepath.Join(isolatedHome, ".cache", "go-build"),
		"GOPATH="+goPath,
		"GOMODCACHE="+filepath.Join(goPath, "pkg", "mod"),
		"GOENV=off",
		"GOWORK=off",
		"GOFLAGS=",
		"GOTOOLCHAIN="+requiredToolchain+"+auto",
		// Asset generation must stay a pure-Go host command. In the
		// Linux/386 route an amd64 Go toolchain intentionally runs inside
		// an i386 userspace whose native C compiler cannot build amd64 cgo.
		"CGO_ENABLED=0",
	)
	if build {
		environment = append(environment, "GOOS="+goos, "GOARCH="+goarch)
		switch goarch {
		case "386":
			environment = append(environment, "GO386=sse2")
		case "amd64":
			environment = append(environment, "GOAMD64=v1")
		case "arm64":
			environment = append(environment, "GOARM64=v8.0")
		}
	}
	return environment
}

func runLogged(ctx context.Context, logger Logger, directory string, environment []string, executable string, arguments ...string) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = directory
	command.Env = environment
	output := &boundedBuffer{limit: 8 << 20}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	if output.Len() > 0 {
		logf(logger, "%s", strings.TrimSpace(output.String()))
	}
	if err != nil {
		if output.truncated {
			return fmt.Errorf("%w (command output truncated)", err)
		}
		return err
	}
	return nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = buffer.Buffer.Write(data)
	}
	if originalLength > remaining {
		buffer.truncated = true
	}
	return originalLength, nil
}

func githubJSON(ctx context.Context, client *http.Client, endpoint, token string, destination any) error {
	data, err := downloadBytes(ctx, client, endpoint, token, maxGitHubJSON)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("GitHub response contains trailing JSON data")
	}
	return nil
}

func apiPath(base string, segments ...string) string {
	result := strings.TrimRight(base, "/")
	for _, segment := range segments {
		result += "/" + url.PathEscape(segment)
	}
	return result
}

func validateGitHubDownloadURL(rawURL, apiBase string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" {
		return errors.New("download URL contains unsupported components")
	}
	api, _ := url.Parse(apiBase)
	if apiBase != defaultGitHubAPI && parsed.Scheme == api.Scheme && parsed.Host == api.Host {
		return nil
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" {
		return errors.New("official release download URL must use https://github.com")
	}
	return nil
}
