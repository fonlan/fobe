package singboxdl

import (
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fonlan/fobe/internal/server/security"
)

// Upstream defaults. Both bases are overridable (Config) so tests and
// air-gapped mirrors can point at a local release server.
const (
	DefaultAPIBase      = "https://api.github.com"
	DefaultDownloadBase = "https://github.com"
	DefaultOwner        = "SagerNet"
	DefaultRepo         = "sing-box"
)

// Cache layout under <DLDir>/singbox/<version>/.
const (
	// DirName is the cache root inside DLDir (matches /dl routes).
	DirName = "singbox"
	// BinaryName is both the on-disk file name and the /dl URL the agent
	// downloads (design §9.2: <DLDir>/singbox/<version>/linux-amd64).
	BinaryName = "linux-amd64"
	// ChecksumName is the sidecar next to the binary, in the sha256sum
	// format: "<hex>  linux-amd64" (two spaces).
	ChecksumName = "linux-amd64.sha256"
	// ManifestName describes a cached version for the panel.
	ManifestName = "manifest.json"
)

// assetSuffix is the locked platform variant: the official single-file static
// musl binary (the standard build ships libcronet.so and is incompatible with
// the agent's single-file install path).
const assetSuffix = "linux-amd64-musl"

// tempPrefix marks in-progress install directories; they are invisible to
// ScanCache and cleaned up by CleanTemps.
const tempPrefix = ".tmp-"

const (
	defaultMaxDownloadBytes = 256 << 20
	defaultHTTPTimeout      = 10 * time.Minute
	userAgent               = "fobe-server"
)

// Errors returned by this package. Callers should use errors.Is.
var (
	// ErrNoDLDir means the server has no artifact directory configured.
	ErrNoDLDir = errors.New("singboxdl: FOBE_DL_DIR is not configured")
	// ErrInvalidVersion means the version/tag is not a usable version.
	ErrInvalidVersion = errors.New("singboxdl: invalid version")
	// ErrVersionExists means the version is already cached (Install without force).
	ErrVersionExists = errors.New("singboxdl: version already cached")
	// ErrNotFound means the requested cached version does not exist.
	ErrNotFound = errors.New("singboxdl: cached version not found")
	// ErrAssetNotFound means the release has no matching musl asset.
	ErrAssetNotFound = errors.New("singboxdl: release asset not found")
	// ErrChecksumUnavailable means neither the API digest nor a sibling
	// .sha256 file could be obtained. Installation is refused (fail-closed).
	ErrChecksumUnavailable = errors.New("singboxdl: no sha256 checksum available")
	// ErrChecksumMismatch means the downloaded artifact does not match the
	// expected digest. Nothing is written to the cache.
	ErrChecksumMismatch = errors.New("singboxdl: sha256 mismatch")
	// ErrBinaryNotFound means the archive did not contain a sing-box binary.
	ErrBinaryNotFound = errors.New("singboxdl: sing-box binary not found in archive")
)

// Config configures a Client. Only DLDir is required.
type Config struct {
	// DLDir is the artifact root (FOBE_DL_DIR, e.g. /srv/dl).
	DLDir string
	// APIBase is the GitHub-compatible API root (default api.github.com).
	APIBase string
	// DownloadBase is the release download root (default github.com) used
	// when an asset carries no explicit browser_download_url.
	DownloadBase string
	// Owner/Repo select the release repository.
	Owner string
	Repo  string
	// HTTPClient overrides the default client (tests use an httptest client).
	HTTPClient *http.Client
	// Log receives download/cleanup notes; nil disables logging.
	Log *slog.Logger
	// Refs reports how many nodes reference a version (desired_version). It
	// is optional and only used to annotate ScanCache results.
	Refs func(version string) int
	// MaxDownloadBytes caps a single asset download (default 256 MiB).
	MaxDownloadBytes int64
	// Progress observes an install in flight (phases + downloaded bytes). It
	// is called from the download loop, so it must be cheap and non-blocking;
	// nil disables reporting entirely.
	Progress func(Progress)
}

// Client talks to the release source and owns the on-disk cache. It is safe
// for concurrent use.
type Client struct {
	cfg       Config
	installMu sync.Mutex
}

// New returns a Client with defaults applied. It never performs I/O.
func New(cfg Config) *Client {
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	if cfg.DownloadBase == "" {
		cfg.DownloadBase = DefaultDownloadBase
	}
	if cfg.Owner == "" {
		cfg.Owner = DefaultOwner
	}
	if cfg.Repo == "" {
		cfg.Repo = DefaultRepo
	}
	if cfg.HTTPClient == nil {
		// Release assets redirect (github.com → objects.githubusercontent.com),
		// so the policy is not "same host" but "every hop still resolves to a
		// routable public address" (§9.5 实现修订 2026-09-20).
		cfg.HTTPClient = security.GuardClient(&http.Client{Timeout: defaultHTTPTimeout})
	}
	if cfg.MaxDownloadBytes <= 0 {
		cfg.MaxDownloadBytes = defaultMaxDownloadBytes
	}
	return &Client{cfg: cfg}
}

// DLDir returns the configured artifact root.
func (c *Client) DLDir() string { return c.cfg.DLDir }

// SingboxDir returns the sing-box cache root (<DLDir>/singbox).
func (c *Client) SingboxDir() string { return filepath.Join(c.cfg.DLDir, DirName) }

// VersionDir returns the directory of one cached version. The version is
// canonicalized first; invalid input yields an empty string.
func (c *Client) VersionDir(version string) string {
	v, err := ParseVersion(version)
	if err != nil {
		return ""
	}
	return filepath.Join(c.SingboxDir(), v.String())
}

// BinaryPath returns the cached binary path for a version.
func (c *Client) BinaryPath(version string) string {
	dir := c.VersionDir(version)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, BinaryName)
}

// AssetName is the official release asset this package installs, e.g.
// "sing-box-1.10.0-linux-amd64-musl.tar.gz".
func AssetName(version string) string {
	return "sing-box-" + version + "-" + assetSuffix + ".tar.gz"
}

func (c *Client) logWarn(msg string, args ...any) {
	if c.cfg.Log != nil {
		c.cfg.Log.Warn(msg, args...)
	}
}

// normalizeVersion parses and canonicalizes a version string.
func normalizeVersion(version string) (Version, string, error) {
	v, err := ParseVersion(strings.TrimSpace(version))
	if err != nil {
		return Version{}, "", err
	}
	return v, v.String(), nil
}
