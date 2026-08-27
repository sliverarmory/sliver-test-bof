package acquire

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

func TestServerExplicitPath(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "sliver-server")
	data := []byte("server fixture")
	if err := os.WriteFile(binaryPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	gotPath, provenance, err := Server(context.Background(), Options{ServerPath: binaryPath})
	if err != nil {
		t.Fatal(err)
	}
	wantPath, _ := filepath.Abs(binaryPath)
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
	if provenance.Selector != "path" || provenance.Digest != sha256Bytes(data) || provenance.Verified {
		t.Fatalf("unexpected provenance: %+v", provenance)
	}
}

func TestCOFFLoaderExplicitPath(t *testing.T) {
	t.Parallel()
	loaderPath := filepath.Join(t.TempDir(), "loader.dll")
	data := []byte("loader fixture")
	if err := os.WriteFile(loaderPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := COFFLoader(context.Background(), Target{OS: "windows", Arch: "amd64"}, COFFLoaderOptions{
		Path: loaderPath, Init: "Initialize", Export: "RunBOF",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loader.Data, data) || loader.Init != "Initialize" || loader.Export != "RunBOF" || loader.Digest != sha256Bytes(data) {
		t.Fatalf("unexpected loader: %+v", loader)
	}
	if _, err := COFFLoader(context.Background(), Target{OS: "linux", Arch: "amd64"}, COFFLoaderOptions{Path: loaderPath}); err == nil {
		t.Fatal("expected unsupported-target error")
	}
}

func TestResolveSelector(t *testing.T) {
	t.Parallel()
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	const tagSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header = %q", request.Header.Get("Authorization"))
		}
		body := ""
		status := http.StatusOK
		switch request.URL.Path {
		case "/repos/BishopFox/sliver/releases/latest":
			body = `{"tag_name":"v1.2.3","assets":[]}`
		case "/repos/BishopFox/sliver/releases/tags/v1.2.3":
			body = `{"tag_name":"v1.2.3","assets":[]}`
		case "/repos/BishopFox/sliver/git/ref/tags/v1.2.3":
			body = fmt.Sprintf(`{"object":{"sha":%q,"type":"tag"}}`, tagSHA)
		case "/repos/BishopFox/sliver/git/tags/" + tagSHA:
			body = fmt.Sprintf(`{"object":{"sha":%q,"type":"commit"}}`, commitSHA)
		case "/repos/BishopFox/sliver/commits/master":
			body = fmt.Sprintf(`{"sha":%q}`, commitSHA)
		default:
			status = http.StatusNotFound
			body = `{"message":"not found"}`
		}
		return &http.Response{
			StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}

	release, err := resolveSelector(context.Background(), "latest", "test-token", client, "https://api.test")
	if err != nil {
		t.Fatal(err)
	}
	if release.Selector != "release:v1.2.3@"+commitSHA || release.ResolvedTag != "v1.2.3" || release.ResolvedSHA != commitSHA || release.SourceBuilt {
		t.Fatalf("unexpected release resolution: %+v", release)
	}
	pinned, err := resolveSelector(context.Background(), "release:v1.2.3@"+commitSHA, "test-token", client, "https://api.test")
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Selector != "release:v1.2.3@"+commitSHA || pinned.ResolvedSHA != commitSHA {
		t.Fatalf("unexpected pinned release resolution: %+v", pinned)
	}
	source, err := resolveSelector(context.Background(), "master", "test-token", client, "https://api.test")
	if err != nil {
		t.Fatal(err)
	}
	if source.Selector != commitSHA || source.ResolvedSHA != commitSHA || !source.SourceBuilt {
		t.Fatalf("unexpected source resolution: %+v", source)
	}
	if _, err := resolveSelector(context.Background(), "release:v1.2.3@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "test-token", client, "https://api.test"); err == nil || !strings.Contains(err.Error(), "expected pinned commit") {
		t.Fatalf("pinned tag movement was not rejected: %v", err)
	}
}

func TestPinnedReleaseSelectorIsStrict(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	tag, gotSHA, ok := parsePinnedReleaseSelector("release:v1.2.3@" + sha)
	if !ok || tag != "v1.2.3" || gotSHA != sha {
		t.Fatalf("parsePinnedReleaseSelector() = %q, %q, %v", tag, gotSHA, ok)
	}
	for _, invalid := range []string{
		"v1.2.3@" + sha,
		"release:v1.2.3@ABCDEF0123456789ABCDEF0123456789ABCDEF01",
		"release:v1.2.3@short",
		"release:1.2.3@" + sha,
	} {
		if isPinnedReleaseSelector(invalid) {
			t.Fatalf("invalid pinned selector %q was accepted", invalid)
		}
	}
}

func TestReleaseAssetIdentityAndDigest(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("ab", 32)
	artifact := githubAsset{ID: 101, Name: "sliver-server_linux-amd64", Size: 10, Digest: "sha256:" + strings.ToUpper(digest)}
	signature := githubAsset{ID: 102, Name: artifact.Name + ".minisig", Size: 10}
	normalized, err := githubAssetSHA256(artifact)
	if err != nil || normalized != digest {
		t.Fatalf("githubAssetSHA256() = %q, %v", normalized, err)
	}
	identity := releaseCacheIdentity("v1.2.3", strings.Repeat("c", 40), "linux", "amd64", artifact, signature, normalized, "")
	changed := artifact
	changed.ID++
	changedIdentity := releaseCacheIdentity("v1.2.3", strings.Repeat("c", 40), "linux", "amd64", changed, signature, normalized, "")
	if identity == changedIdentity {
		t.Fatal("release asset ID did not affect cache identity")
	}
	artifact.Digest = "sha512:" + digest
	if _, err := githubAssetSHA256(artifact); err == nil {
		t.Fatal("unsupported release asset digest was accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestMinisignVerification(t *testing.T) {
	t.Parallel()
	data := []byte("signed artifact")
	publicKey, signature := signMinisignFixture(t, data, "timestamp:1\tfile:artifact")
	trusted, err := verifyMinisignBytes(publicKey, data, signature)
	if err != nil {
		t.Fatal(err)
	}
	if trusted != "timestamp:1\tfile:artifact" {
		t.Fatalf("trusted comment = %q", trusted)
	}
	if _, err := verifyMinisignBytes(publicKey, append(data, '!'), signature); err == nil {
		t.Fatal("tampered artifact unexpectedly verified")
	}
	trusted, err = verifyMinisignReader(publicKey, bytes.NewReader(data), signature)
	if err != nil || trustedCommentFile(trusted) != "artifact" {
		t.Fatalf("stream verification = %q, %v", trusted, err)
	}
}

func TestSourceArchiveRejectsTraversal(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "source.tar.gz")
	writeTarGzipFixture(t, archive, []tar.Header{
		{Name: "repo/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "repo/../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
	}, [][]byte{nil, []byte("x")})
	if err := extractSourceTarGzip(archive, t.TempDir()); err == nil {
		t.Fatal("path traversal unexpectedly extracted")
	}
}

func TestSourceArchiveRejectsDirectoryBody(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(archive, directoryBodyTarGzipFixture(t, "repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractSourceTarGzip(archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), "non-zero size") {
		t.Fatalf("directory body was not rejected: %v", err)
	}
}

func TestSourceArchiveAcceptsBoundedPAXGlobalHeader(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "source.tar.gz")
	writeTarGzipFixture(t, archive, []tar.Header{
		{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "fixture"}},
		{Name: "repo/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "repo/go.mod", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	}, [][]byte{nil, nil, []byte("mod")})
	destination := t.TempDir()
	if err := extractSourceTarGzip(archive, destination); err != nil {
		t.Fatalf("extract source with PAX global header: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "go.mod")); err != nil || string(data) != "mod" {
		t.Fatalf("extracted go.mod = %q, %v", data, err)
	}
}

func TestCacheDigestIsValidated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "test-identity"
	entry := cacheEntry(root, "server", identity)
	provenance := Provenance{Selector: "v1.0.0", Digest: sha256Bytes([]byte("good")), Verified: true}
	path, _, _, err := storeCache(entry, "server", identity, "linux", "amd64", source, provenance, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := restoreCache(entry, "server", identity, "linux", "amd64"); !ok {
		t.Fatal("valid cache entry was not restored")
	}
	if err := os.WriteFile(path, []byte("bad"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := restoreCache(entry, "server", identity, "linux", "amd64"); ok {
		t.Fatal("cache entry with a mismatched digest was restored")
	}
}

func TestStoreCacheRestoresValidWinner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("winner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("loser"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "publication-race"
	entry := cacheEntry(root, "server", identity)
	winnerProvenance := Provenance{Digest: sha256Bytes([]byte("winner"))}
	if _, _, _, err := storeCache(entry, "server", identity, "linux", "amd64", first, winnerProvenance, []byte("winner-signature")); err != nil {
		t.Fatal(err)
	}
	path, restored, signature, err := storeCache(entry, "server", identity, "linux", "amd64", second, Provenance{Digest: sha256Bytes([]byte("loser"))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "winner" || restored.Digest != winnerProvenance.Digest || !restored.CacheHit || string(signature) != "winner-signature" {
		t.Fatalf("valid winner was not restored: data=%q provenance=%+v signature=%q", data, restored, signature)
	}
}

func TestStoreCacheRefusesInvalidExistingEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := cacheEntry(root, "server", "invalid-existing")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(entry, "do-not-delete")
	if err := os.WriteFile(marker, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := storeCache(entry, "server", "invalid-existing", "linux", "amd64", source, Provenance{Digest: sha256Bytes([]byte("new"))}, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("invalid existing cache error = %v", err)
	}
	if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "existing" {
		t.Fatalf("existing cache entry was altered: %q, %v", data, readErr)
	}
}

func TestCacheRootUsesRunnerTemp(t *testing.T) {
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	root, err := cacheRoot("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(runnerTemp, "sliver-bof-e2e-cache")
	if root != want {
		t.Fatalf("cacheRoot() = %q, want %q", root, want)
	}
}

func TestCachedReleaseSignatureIsReverified(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	input := filepath.Join(root, "input")
	if err := os.Mkdir(input, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(input, "sliver-server")
	data := []byte("signed cached server")
	if err := os.WriteFile(artifact, data, 0o700); err != nil {
		t.Fatal(err)
	}
	publicKey, signature := signMinisignFixture(t, data, "timestamp:1\tfile:sliver-server_linux-amd64")
	identity := "release|v1.0.0|commit|linux|amd64"
	entry := cacheEntry(root, "server", identity)
	provenance := Provenance{Selector: "v1.0.0", Digest: sha256Bytes(data), Verified: true}
	path, _, _, err := storeCache(entry, "server", identity, "linux", "amd64", artifact, provenance, signature)
	if err != nil {
		t.Fatal(err)
	}
	restored, _, restoredSignature, ok := restoreCache(entry, "server", identity, "linux", "amd64")
	if !ok || restored != path {
		t.Fatal("signed cache entry was not restored")
	}
	if err := verifyCachedServerWithKey(restored, restoredSignature, "sliver-server_linux-amd64", publicKey); err != nil {
		t.Fatalf("verify valid cached release: %v", err)
	}
	if err := verifyCachedServerWithKey(restored, restoredSignature, "sliver-server_linux-arm64", publicKey); err == nil {
		t.Fatal("signature with the wrong trusted file field was accepted")
	}
	restoredSignature[len(restoredSignature)-1] ^= 1
	if err := verifyCachedServerWithKey(restored, restoredSignature, "sliver-server_linux-amd64", publicKey); err == nil {
		t.Fatal("corrupt cached signature was accepted")
	}
}

func TestCacheMetadataIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	identity := "oversized-metadata"
	entry := cacheEntry(root, "server", identity)
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "metadata.json"), bytes.Repeat([]byte("x"), maxCacheMetadata+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := restoreCache(entry, "server", identity, "linux", "amd64"); ok {
		t.Fatal("cache entry with oversized metadata was restored")
	}
}

func TestPackageArchiveRejectsLinks(t *testing.T) {
	t.Parallel()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "loader.dll", Typeflag: tar.TypeSymlink, Linkname: "../../escape"}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readPackageTarGzip(archive.Bytes()); err == nil {
		t.Fatal("package symlink unexpectedly accepted")
	}
}

func TestPackageArchiveRejectsDirectoryBody(t *testing.T) {
	t.Parallel()
	if _, err := readPackageTarGzip(directoryBodyTarGzipFixture(t, "directory")); err == nil || !strings.Contains(err.Error(), "non-zero size") {
		t.Fatalf("directory body was not rejected: %v", err)
	}
}

func TestCustomCOFFLoaderMirrorDoesNotReceiveGitHubToken(t *testing.T) {
	t.Parallel()
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("custom mirror received authorization header %q", authorization)
		}
		return &http.Response{
			StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("not found")), Request: request,
		}, nil
	})}
	_, err := COFFLoader(context.Background(), Target{OS: "windows", Arch: "amd64"}, COFFLoaderOptions{
		Token: "github-secret", ArchiveURL: "https://mirror.test/archive", SignatureURL: "https://mirror.test/signature", HTTPClient: client,
	})
	if err == nil || requests == 0 {
		t.Fatalf("custom mirror acquisition = %v, requests=%d", err, requests)
	}
}

func TestLoaderManifestParsingIsStrict(t *testing.T) {
	t.Parallel()
	valid := []byte(`{
		"name":"COFF Loader",
		"command_name":"coff-loader",
		"version":"v1.0.16",
		"extension_author":"lesnuages",
		"original_author":"TrustedSec",
		"repo_url":"https://github.com/sliverarmory/COFFLoader",
		"help":"fixture",
		"entrypoint":"LoadAndRun",
		"files":[{"os":"windows","arch":"amd64","path":"COFFLoader.x64.dll"}]
	}`)
	manifest, err := parseLoaderManifest(valid)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := exactLoaderPath(manifest, Target{OS: "windows", Arch: "amd64"}); err != nil || path != "COFFLoader.x64.dll" {
		t.Fatalf("exact loader path = %q, %v", path, err)
	}
	unknown := bytes.Replace(valid, []byte(`"help":"fixture"`), []byte(`"help":"fixture","unexpected":true`), 1)
	if _, err := parseLoaderManifest(unknown); err == nil {
		t.Fatal("unknown signed manifest field was accepted")
	}
	trailing := append(append([]byte(nil), valid...), []byte(` {}`)...)
	if _, err := parseLoaderManifest(trailing); err == nil {
		t.Fatal("trailing signed manifest document was accepted")
	}
}

func TestCommandEnvPinsSourceToolchainAndCrossBuild(t *testing.T) {
	runnerTemp := t.TempDir()
	ambientGoRoot := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	t.Setenv("GOOS", "attacker-os")
	t.Setenv("GOARCH", "attacker-arch")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GOCACHE", filepath.Join(ambientGoRoot, "cache"))
	t.Setenv("GOMODCACHE", filepath.Join(ambientGoRoot, "mod"))
	t.Setenv("GOPATH", ambientGoRoot)
	t.Setenv("GoCache", filepath.Join(ambientGoRoot, "case-variant-cache"))
	t.Setenv("GoModCache", filepath.Join(ambientGoRoot, "case-variant-mod"))
	t.Setenv("GoPath", filepath.Join(ambientGoRoot, "case-variant-path"))
	// A newer launcher/default must not silently compile Sliver with a newer
	// standard library than the vendored source expects.
	t.Setenv("GOTOOLCHAIN", "go1.27.0+auto")
	t.Setenv("GOFLAGS", "-toolexec=attacker")
	t.Setenv("GOWORK", "/tmp/attacker.work")
	t.Setenv("GOENV", "/tmp/attacker.env")
	t.Setenv("GOAMD64", "v4")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "must-not-leak")
	t.Setenv("LocalAppData", filepath.Join(ambientGoRoot, "local-app-data"))
	t.Setenv("APPDATA", filepath.Join(ambientGoRoot, "roaming-app-data"))
	environment := commandEnv(true, "windows", "amd64", "go1.26.6")
	isolatedHome := filepath.Join(runnerTemp, "sliver-bof-e2e-go-home")
	want := map[string]string{
		"GOOS": "windows", "GOARCH": "amd64", "CGO_ENABLED": "0", "GOTOOLCHAIN": "go1.26.6+auto",
		"GOAMD64": "v1", "GOFLAGS": "", "GOWORK": "off", "GOENV": "off",
		"HOME": isolatedHome, "USERPROFILE": isolatedHome,
		"GOCACHE":    filepath.Join(isolatedHome, ".cache", "go-build"),
		"GOPATH":     filepath.Join(isolatedHome, "go"),
		"GOMODCACHE": filepath.Join(isolatedHome, "go", "pkg", "mod"),
	}
	seen := make(map[string]struct{}, len(environment))
	for _, item := range environment {
		name, value, _ := strings.Cut(item, "=")
		name = strings.ToUpper(name)
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("duplicate case-insensitive environment entry %s", name)
		}
		seen[name] = struct{}{}
		if name == "GITHUB_TOKEN" || name == "ACTIONS_RUNTIME_TOKEN" || name == "LOCALAPPDATA" || name == "APPDATA" {
			t.Fatalf("%s leaked into source build environment", name)
		}
		if expected, ok := want[name]; ok {
			if value != expected {
				t.Fatalf("%s = %q, want %q", name, value, expected)
			}
			delete(want, name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing command environment entries: %v", want)
	}
}

func TestCommandEnvProvidesPrivateCachesWithoutWindowsProfile(t *testing.T) {
	runnerTemp := t.TempDir()
	environment := commandEnvFrom([]string{
		"Path=/trusted/tools",
		"Runner_Temp=" + runnerTemp,
		"GITHUB_TOKEN=github-secret",
		"ACTIONS_RUNTIME_TOKEN=actions-secret",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN=oidc-secret",
		"AWS_SECRET_ACCESS_KEY=cloud-secret",
		"GIT_CONFIG_COUNT=1",
		"GOAUTH=netrc",
		"SSH_AUTH_SOCK=/private/agent.sock",
		// GOCACHE and LocalAppData are intentionally absent, matching the
		// Windows runner failure that reached the asset-preparation command.
	}, false, "", "", "go1.26.6")
	values := uniqueEnvironment(t, environment)
	isolatedHome := filepath.Join(runnerTemp, "sliver-bof-e2e-go-home")
	want := map[string]string{
		"PATH":            "/trusted/tools",
		"HOME":            isolatedHome,
		"USERPROFILE":     isolatedHome,
		"XDG_CONFIG_HOME": filepath.Join(isolatedHome, ".config"),
		"GOCACHE":         filepath.Join(isolatedHome, ".cache", "go-build"),
		"GOPATH":          filepath.Join(isolatedHome, "go"),
		"GOMODCACHE":      filepath.Join(isolatedHome, "go", "pkg", "mod"),
		"CGO_ENABLED":     "0",
	}
	for name, expected := range want {
		if got := values[name]; got != expected {
			t.Errorf("%s = %q, want %q", name, got, expected)
		}
	}
	for _, name := range []string{
		"LOCALAPPDATA", "APPDATA", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN", "AWS_SECRET_ACCESS_KEY", "GIT_CONFIG_COUNT",
		"GOAUTH", "SSH_AUTH_SOCK", "GOOS", "GOARCH",
	} {
		if _, ok := values[name]; ok {
			t.Errorf("%s leaked into asset-preparation environment", name)
		}
	}
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "GOCACHE", "GOPATH", "GOMODCACHE"} {
		value := values[name]
		if !filepath.IsAbs(value) {
			t.Errorf("%s = %q, want absolute path", name, value)
			continue
		}
		relative, err := filepath.Rel(runnerTemp, value)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Errorf("%s = %q, want path contained by RUNNER_TEMP %q", name, value, runnerTemp)
		}
	}
}

func TestCommandEnvKeepsLinux386PreparationHostOnly(t *testing.T) {
	runnerTemp := t.TempDir()
	host := []string{
		"PATH=/usr/local/go/bin:/usr/bin:/bin",
		"RUNNER_TEMP=" + runnerTemp,
		"GOOS=linux",
		"GOARCH=386",
		"CGO_ENABLED=1",
		"CC=/usr/bin/i686-linux-gnu-gcc",
	}
	assetEnvironment := uniqueEnvironment(t, commandEnvFrom(host, false, "", "", "go1.26.6"))
	if assetEnvironment["CGO_ENABLED"] != "0" {
		t.Fatalf("asset CGO_ENABLED = %q, want 0", assetEnvironment["CGO_ENABLED"])
	}
	for _, name := range []string{"GOOS", "GOARCH", "CC"} {
		if _, ok := assetEnvironment[name]; ok {
			t.Errorf("asset-preparation environment inherited target setting %s", name)
		}
	}

	serverEnvironment := uniqueEnvironment(t, commandEnvFrom(host, true, "linux", "amd64", "go1.26.6"))
	for name, expected := range map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      "amd64",
	} {
		if got := serverEnvironment[name]; got != expected {
			t.Errorf("server %s = %q, want %q", name, got, expected)
		}
	}
	if _, ok := serverEnvironment["CC"]; ok {
		t.Error("server build inherited the i386 userspace C compiler")
	}
}

func uniqueEnvironment(t *testing.T, environment []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		name, value, found := strings.Cut(item, "=")
		if !found || strings.TrimSpace(name) == "" {
			t.Fatalf("invalid environment entry %q", item)
		}
		name = strings.ToUpper(name)
		if _, duplicate := values[name]; duplicate {
			t.Fatalf("duplicate case-insensitive environment entry %s", name)
		}
		values[name] = value
	}
	return values
}

func TestRequiredGoToolchainPinsModuleVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		mod   string
		want  string
		fails bool
	}{
		{name: "exact current Sliver version", mod: "module github.com/bishopfox/sliver\n\ngo 1.26.6\n", want: "go1.26.6"},
		{name: "toolchain directive wins", mod: "module github.com/bishopfox/sliver\n\ngo 1.26.6\ntoolchain go1.26.7\n", want: "go1.26.7"},
		{name: "new language version gets dot zero", mod: "module github.com/bishopfox/sliver\n\ngo 1.25\n", want: "go1.25.0"},
		{name: "legacy language version", mod: "module github.com/bishopfox/sliver\n\ngo 1.20\n", want: "go1.20"},
		{name: "nonstandard toolchain rejected", mod: "module github.com/bishopfox/sliver\n\ngo 1.26.6\ntoolchain corp-go\n", fails: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := requiredGoToolchain([]byte(test.mod))
			if test.fails {
				if err == nil {
					t.Fatalf("requiredGoToolchain() = %q, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("requiredGoToolchain() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func signMinisignFixture(t *testing.T, data []byte, trustedComment string) (string, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = uint64(0x0102030405060708)
	publicRaw := make([]byte, 2+8+ed25519.PublicKeySize)
	binary.LittleEndian.PutUint16(publicRaw[:2], minisignEdDSA)
	binary.LittleEndian.PutUint64(publicRaw[2:10], keyID)
	copy(publicRaw[10:], public)
	publicText := base64.StdEncoding.EncodeToString(publicRaw)
	digest := blake2b.Sum512(data)
	artifactSignature := ed25519.Sign(private, digest[:])
	raw := make([]byte, 2+8+ed25519.SignatureSize)
	binary.LittleEndian.PutUint16(raw[:2], minisignHashEdDSA)
	binary.LittleEndian.PutUint64(raw[2:10], keyID)
	copy(raw[10:], artifactSignature)
	commentMessage := append(append([]byte(nil), artifactSignature...), trustedComment...)
	commentSignature := ed25519.Sign(private, commentMessage)
	signatureText := strings.Join([]string{
		"untrusted comment: test fixture",
		base64.StdEncoding.EncodeToString(raw),
		"trusted comment: " + trustedComment,
		base64.StdEncoding.EncodeToString(commentSignature),
	}, "\n")
	return publicText, []byte(signatureText)
}

func writeTarGzipFixture(t *testing.T, destination string, headers []tar.Header, contents [][]byte) {
	t.Helper()
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for index := range headers {
		if err := tarWriter.WriteHeader(&headers[index]); err != nil {
			t.Fatal(err)
		}
		if len(contents[index]) > 0 {
			if _, err := tarWriter.Write(contents[index]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func directoryBodyTarGzipFixture(t *testing.T, name string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tarWriter := tar.NewWriter(&raw)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	header := raw.Bytes()[:512]
	header[156] = tar.TypeDir
	for index := 148; index < 156; index++ {
		header[index] = ' '
	}
	checksum := 0
	for _, value := range header {
		checksum += int(value)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
