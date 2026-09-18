package modelsdev

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Downloading and installing the cache. Every step keeps the previous state
// intact on failure: a broken mirror, a truncated body or an HTML error page
// must never displace a working cache file or the index in memory (§12.5).

// fetchAndInstall downloads the document into a temp file, parses it there and
// only then moves it onto the cache path.
//
// 顺序很重要（照 §14.1 GeoIP 的"安装必须能解析"）：先落临时文件 → 在临时文件上
// 解析 → 解析通过才 rename → 换内存索引。任何一个 200 但其实是错误页/被截断的响应
// 都倒在解析这一步，线上缓存与内存索引原封不动。
func (m *Manager) fetchAndInstall(ctx context.Context) error {
	path := m.Path()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return m.fail(fmt.Errorf("create models cache dir %s: %w", dir, err))
	}

	tmp, err := os.CreateTemp(dir, cacheFileName+".tmp-*")
	if err != nil {
		return m.fail(fmt.Errorf("create models cache temp file in %s: %w", dir, err))
	}
	tmpName := tmp.Name()
	// No-op once the rename below succeeded; the cleanup path otherwise.
	defer os.Remove(tmpName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if err != nil {
		tmp.Close()
		return m.fail(fmt.Errorf("build models request: %w", err))
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		tmp.Close()
		return m.fail(fmt.Errorf("fetch %s: %w", m.url, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return m.fail(fmt.Errorf("fetch %s: unexpected http %d", m.url, resp.StatusCode))
	}

	// LimitReader with one extra byte: exceeding the cap is detected without
	// being able to stream forever.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, m.cfg.MaxBytes+1))
	if err != nil {
		tmp.Close()
		return m.fail(fmt.Errorf("download %s: %w", m.url, err))
	}
	if err := tmp.Close(); err != nil {
		return m.fail(fmt.Errorf("write models cache temp file: %w", err))
	}
	if n > m.cfg.MaxBytes {
		return m.fail(fmt.Errorf("download %s (%d bytes): %w", m.url, n, ErrTooLarge))
	}

	// Parse before installing: the cache on disk keeps serving until the new
	// document is proven readable.
	ix, err := parseFile(tmpName)
	if err != nil {
		return m.fail(fmt.Errorf("parse downloaded models document: %w", err))
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return m.fail(fmt.Errorf("chmod models cache temp file: %w", err))
	}
	if err := os.Rename(tmpName, path); err != nil {
		return m.fail(fmt.Errorf("install models cache %s: %w", path, err))
	}

	m.swap(ix)
	if info, err := os.Stat(path); err == nil {
		m.setUpdated(info.ModTime())
	}
	m.logInfo("modelsdev: metadata refreshed",
		"url", m.url, "path", path, "bytes", n,
		"providers", ix.ProviderCount(), "models", ix.ModelCount())
	return nil
}

// parseFile parses a local document.
func parseFile(name string) (*Index, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	return Parse(f)
}
