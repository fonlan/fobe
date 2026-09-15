package singboxdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxReleasePages caps how many API pages we walk when looking for a version
// (100 releases per page, newest first).
const maxReleasePages = 5

// maxJSONBytes bounds a single API response body.
const maxJSONBytes = 8 << 20

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
	Assets     []Asset
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
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(out); err != nil {
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

// ListReleases walks the releases listing (newest first) and returns every
// published, version-parseable release.
func (c *Client) ListReleases(ctx context.Context) ([]Release, error) {
	next := c.releasesURL() + "?per_page=100"
	var out []Release
	for page := 0; page < maxReleasePages && next != ""; page++ {
		var raw []githubRelease
		hdr, err := c.getJSON(ctx, next, &raw)
		if err != nil {
			return nil, err
		}
		for _, r := range raw {
			if r.Draft {
				continue
			}
			v, err := ParseVersion(r.TagName)
			if err != nil {
				continue
			}
			out = append(out, Release{
				Version:    v.String(),
				Tag:        r.TagName,
				Prerelease: r.Prerelease || !v.IsStable(),
				Assets:     r.Assets,
			})
		}
		next = c.nextPageURL(hdr.Get("Link"))
	}
	return out, nil
}

// LatestStable returns the highest stable release (drafts and prereleases
// excluded). It is what the startup auto-download installs.
func (c *Client) LatestStable(ctx context.Context) (Release, error) {
	releases, err := c.ListReleases(ctx)
	if err != nil {
		return Release{}, err
	}
	var best Release
	found := false
	for _, r := range releases {
		if r.Prerelease {
			continue
		}
		if !found || CompareVersions(r.Version, best.Version) > 0 {
			best, found = r, true
		}
	}
	if !found {
		return Release{}, fmt.Errorf("singboxdl: no stable release found for %s/%s", c.cfg.Owner, c.cfg.Repo)
	}
	return best, nil
}

// ReleaseByVersion looks a specific version up in the release listing. The
// version may carry a leading "v"; it is canonicalized before matching.
func (c *Client) ReleaseByVersion(ctx context.Context, version string) (Release, error) {
	_, canonical, err := normalizeVersion(version)
	if err != nil {
		return Release{}, err
	}
	releases, err := c.ListReleases(ctx)
	if err != nil {
		return Release{}, err
	}
	for _, r := range releases {
		if r.Version == canonical {
			return r, nil
		}
	}
	return Release{}, fmt.Errorf("singboxdl: release %s not found: %w", canonical, ErrNotFound)
}
