// Package acquire downloads and verifies the external tools used by the BOF
// end-to-end harness.
package acquire

import (
	"net/http"
)

// Logger is the small logging surface used while acquiring or building tools.
// *log.Logger and testing.T-compatible adapters can implement it directly.
type Logger interface {
	Printf(format string, args ...any)
}

// Options controls Sliver server acquisition.
type Options struct {
	// Version accepts latest, a v-prefixed release tag, an internally pinned
	// release:vX.Y.Z@<commit-sha> selector, master, or a full 40-character
	// commit SHA. It defaults to master.
	Version string
	// ServerPath bypasses network acquisition and uses an existing binary.
	ServerPath string
	CacheDir   string
	Token      string
	HostOS     string
	HostArch   string
	Logger     Logger

	// HTTPClient and GitHubAPIURL primarily support deterministic tests and
	// GitHub Enterprise mirrors. They may be left unset by normal callers.
	HTTPClient   *http.Client
	GitHubAPIURL string
}

// Provenance describes exactly what was selected and returned by an
// acquisition operation.
type Provenance struct {
	Selector    string `json:"selector"`
	ResolvedTag string `json:"resolved_tag,omitempty"`
	ResolvedSHA string `json:"resolved_sha,omitempty"`
	Digest      string `json:"sha256"`
	SourceBuilt bool   `json:"source_built"`
	CacheHit    bool   `json:"cache_hit"`
	Verified    bool   `json:"verified"`
	Source      string `json:"source,omitempty"`
	Toolchain   string `json:"toolchain,omitempty"`
}

// Resolution pins a mutable selector before matrix jobs are dispatched.
// Selector is the value child jobs should use: a release tag plus its backing
// commit for a release binary, or a full commit SHA for a source build.
type Resolution struct {
	Requested   string `json:"requested"`
	Selector    string `json:"selector"`
	ResolvedTag string `json:"resolved_tag,omitempty"`
	ResolvedSHA string `json:"resolved_sha"`
	SourceBuilt bool   `json:"source_built"`
}

// Target is an implant target, not necessarily the architecture of the host
// running sliver-server.
type Target struct {
	OS   string
	Arch string
}

// COFFLoaderOptions controls legacy COFFLoader acquisition.
type COFFLoaderOptions struct {
	// Path uses a caller-supplied loader instead of the pinned official package.
	Path     string
	CacheDir string
	Token    string
	Logger   Logger
	Init     string
	Export   string

	// HTTPClient, ArchiveURL, and SignatureURL are useful for mirrors and tests.
	// When either URL is changed, the pinned transport SHA-256 values are not
	// applied, but the package must still pass its minisign trust chain.
	HTTPClient   *http.Client
	ArchiveURL   string
	SignatureURL string
}

// Loader is the material needed to register and invoke a legacy COFFLoader.
type Loader struct {
	Data       []byte
	Init       string
	Export     string
	Digest     string
	Provenance Provenance
}

func logf(logger Logger, format string, args ...any) {
	if logger != nil {
		logger.Printf(format, args...)
	}
}
