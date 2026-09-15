package singboxdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxReleasePages caps how many API pages we walk when looking for a version
// (newest first).
const maxReleasePages = 5

// Page sizes. A release listing entry is heavy (~300 KB of JSON per release:
// every release repeats ~170 assets, each with URL, digest and uploader), so
// the "which stable release is current" fast path asks for fewer entries per
// page: 30 already spans months of sing-box releases, and a page carrying no
// stable release simply makes the walk continue.
const (
	listPageSize   = 100
	newestPageSize = 30
)

// maxJSONBytes bounds a single API response body. It guards against a mirror
// that streams forever — it is NOT a payload assertion, and it has to clear
// the real listing by a wide margin: the releases endpoint repeats every
// asset (name, URL, digest, size, uploader) of every release, so
// SagerNet/sing-box at ?per_page=100 answers with ~33 MB uncompressed today
// (~2 MB gzipped; each release is ~300 KB / ~170 assets). A cap under that
// truncates the body mid-array, which the decoder reports as "unexpected EOF"
// — a network-fault message for what is really a size problem.
const maxJSONBytes = 64 << 20

// Asset is one downloadable file of a release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	// Digest is "sha256:<hex>" when the API provides it (GitHub does for
	// newer releases); empty otherwise.
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Release is a version-centric view of an upstream release. Drafts are
// dropped; prereleases are kept but flagged.
type Release struct {
	// Version is the canonical version without the leading "v" (1.10.0).
	Version string
	// Tag is the raw upstream tag ("v1.10.0").
	Tag        string
	Prerelease bool
	// PublishedAt is the upstream publication time (unix seconds, 0 when the
	// API (or a mirror) did not provide it). Panel-only metadata: the version
	// picker shows it so an operator can tell a fresh stable from an old one.
	PublishedAt int64
	Assets      []Asset
}

// Asset returns the named asset of the release.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// githubRelease mirrors the subset of the GitHub releases API we consume.
type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// releasesURL is the (paginated) releases listing endpoint.
func (c *Client) releasesURL() string {
	return strings.TrimRight(c.cfg.APIBase, "/") + "/repos/" +
		url.PathEscape(c.cfg.Owner) + "/" + url.PathEscape(c.cfg.Repo) + "/releases"
}

// assetURL resolves where to download an asset: the API-provided browser URL
// when present, otherwise the conventional release path under DownloadBase.
func (c *Client) assetURL(rel Release, a Asset) string {
	if a.BrowserDownloadURL != "" {
		return a.BrowserDownloadURL
	}
	return strings.TrimRight(c.cfg.DownloadBase, "/") + "/" + url.PathEscape(c.cfg.Owner) + "/" +
		url.PathEscape(c.cfg.Repo) + "/releases/download/" + url.PathEscape(rel.Tag) + "/" + url.PathEscape(a.Name)
}

// decodeJSONBody decodes one JSON value from body, reading at most limit
// bytes. Exhausting the limit is reported as such: a bare io.LimitReader
// makes json.Decoder fail with io.ErrUnexpectedEOF, which reads like a
// truncated download and sends whoever inspects the log hunting for a network
// problem that is not there.
func decodeJSONBody(body io.Reader, limit int64, out any) error {
	lr := &io.LimitedReader{R: body, N: limit}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		if lr.N <= 0 {
			return fmt.Errorf("response exceeds %d bytes", limit)
		}
		return err
	}
	return nil
}

// getJSON performs an API GET and decodes the body, returning the response
// headers (the Link header drives pagination).
func (c *Client) getJSON(ctx context.Context, u string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("singboxdl: request %s: %w", u, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("singboxdl: get %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.Header, fmt.Errorf("singboxdl: get %s: %s", u, resp.Status)
	}
	if err := decodeJSONBody(resp.Body, maxJSONBytes, out); err != nil {
		return resp.Header, fmt.Errorf("singboxdl: decode %s: %w", u, err)
	}
	return resp.Header, nil
}

// nextPageURL extracts rel="next" from a Link header, but only when it stays
// on the configured API origin (a mirror's pagination must not redirect us
// to an arbitrary host).
func (c *Client) nextPageURL(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		target := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		next := false
		for _, s := range segs[1:] {
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "rel=") && strings.Contains(s, "next") {
				next = true
			}
		}
		if !next {
			continue
		}
		target = target[1 : len(target)-1]
		if strings.HasPrefix(target, strings.TrimRight(c.cfg.APIBase, "/")+"/") {
			return target
		}
	}
	return ""
}

// eachReleasePage walks the releases listing (newest first) page by page and
// hands every page to fn, which reports whether it has seen enough. Stopping
// early matters: one page of 100 releases is ~33 MB uncompressed, so callers
// that only need the newest stable release (or one specific version) must not
// read all maxReleasePages pages — the panel's "latest" lookup has a 20 s
// budget and the full walk alone takes most of it.
func (c *Client) eachReleasePage(ctx context.Context, perPage int, fn func(page []Release) (done bool, err error)) error {
	next := fmt.Sprintf("%s?per_page=%d", c.releasesURL(), perPage)
	for page := 0; page < maxReleasePages && next != ""; page++ {
		var raw []githubRelease
		hdr, err := c.getJSON(ctx, next, &raw)
		if err != nil {
			return err
		}
		out := make([]Release, 0, len(raw))
		for _, r := range raw {
			if r.Draft {
				continue
			}
			v, err := ParseVersion(r.TagName)
			if err != nil {
				continue
			}
			// A zero time (mirror without published_at) must not become the
			// year-1 epoch in the panel.
			published := int64(0)
			if !r.PublishedAt.IsZero() {
				published = r.PublishedAt.Unix()
			}
			out = append(out, Release{
				Version:     v.String(),
				Tag:         r.TagName,
				Prerelease:  r.Prerelease || !v.IsStable(),
				PublishedAt: published,
				Assets:      r.Assets,
			})
		}
		done, err := fn(out)
		if err != nil || done {
			return err
		}
		next = c.nextPageURL(hdr.Get("Link"))
	}
	return nil
}

// ListReleases walks the releases listing (newest first) and returns every
// published, version-parseable release.
func (c *Client) ListReleases(ctx context.Context) ([]Release, error) {
	var out []Release
	err := c.eachReleasePage(ctx, listPageSize, func(page []Release) (bool, error) {
		out = append(out, page...)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecentReleases returns the newest `limit` releases, newest first, from a
// single API page — it never walks the pagination.
//
// That bound is the point: one page of 100 releases answers with ~33 MB
// uncompressed, and the whole listing is months of history. A version picker
// only needs the head of the list, so paying for a full walk (seconds of
// decoding per extra page) to render a dropdown is not acceptable. Callers
// that need a specific old version use ReleaseByVersion, which stops on the
// page that carries it.
func (c *Client) RecentReleases(ctx context.Context, limit int) ([]Release, error) {
	if limit <= 0 || limit > listPageSize {
		limit = newestPageSize
	}
	var out []Release
	err := c.eachReleasePage(ctx, limit, func(page []Release) (bool, error) {
		out = append(out, page...)
		return true, nil // one page is the whole answer
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LatestStable returns the highest stable release (drafts and prereleases
// excluded). It is what the startup auto-download installs.
//
// It stops at the first page that carries a stable release: pages are ordered
// by creation date, and a stable release can only be outranked by one created
// later, i.e. one on an earlier page. (An old line's backport is created later
// still, but its version is numerically lower, so it cannot win the max.)
func (c *Client) LatestStable(ctx context.Context) (Release, error) {
	var best Release
	found := false
	err := c.eachReleasePage(ctx, newestPageSize, func(page []Release) (bool, error) {
		stable := false
		for _, r := range page {
			if r.Prerelease {
				continue
			}
			stable = true
			if !found || CompareVersions(r.Version, best.Version) > 0 {
				best, found = r, true
			}
		}
		return stable, nil
	})
	if err != nil {
		return Release{}, err
	}
	if !found {
		return Release{}, fmt.Errorf("singboxdl: no stable release found for %s/%s", c.cfg.Owner, c.cfg.Repo)
	}
	return best, nil
}

// ReleaseByVersion looks a specific version up in the release listing. The
// version may carry a leading "v"; it is canonicalized before matching. The
// walk stops on the matching page instead of reading the whole history.
func (c *Client) ReleaseByVersion(ctx context.Context, version string) (Release, error) {
	_, canonical, err := normalizeVersion(version)
	if err != nil {
		return Release{}, err
	}
	var hit Release
	hitFound := false
	err = c.eachReleasePage(ctx, listPageSize, func(page []Release) (bool, error) {
		for _, r := range page {
			if r.Version == canonical {
				hit, hitFound = r, true
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return Release{}, err
	}
	if !hitFound {
		return Release{}, fmt.Errorf("singboxdl: release %s not found: %w", canonical, ErrNotFound)
	}
	return hit, nil
}
