// Conditional-request cache. After an -o download the remote's ETag and/or
// Last-Modified go to a .gp-meta sidecar; the next run for the same URL
// replays them, so an unchanged file returns 304 (--force opts out). On a
// 200 the same request is the size/range probe, so no extra round trip.
package base

import (
	"net/http"
	"os"
)

const metaSuffix = ".gp-meta"

// URL guards against replaying a validator for another source written to
// the same path; Size against a file truncated since (a killed --force run).
type meta struct {
	URL     string `json:"url"`
	Size    int64  `json:"size"`
	ETag    string `json:"etag,omitempty"`
	LastMod string `json:"last_modified,omitempty"`
}

func metaPath(out string) string { return out + metaSuffix }

func loadMeta(path string) (meta, bool) {
	var m meta
	return m, readJSON(path, &m)
}

// No validator means nothing to replay later: ok is false.
func metaFrom(url string, resp *http.Response, out string) (meta, bool) {
	fi, err := os.Stat(out)
	if err != nil {
		return meta{}, false
	}
	m := meta{
		URL:     url,
		Size:    fi.Size(),
		ETag:    resp.Header.Get("ETag"),
		LastMod: resp.Header.Get("Last-Modified"),
	}
	if m.ETag == "" && m.LastMod == "" {
		return meta{}, false
	}
	return m, true
}

func (m meta) save(path string) error { return writeJSONAtomic(path, m) }

// Per RFC 9110 the server prefers If-None-Match when both are sent.
func (m meta) applyConditional(req *http.Request) {
	if m.ETag != "" {
		req.Header.Set("If-None-Match", m.ETag)
	}
	if m.LastMod != "" {
		req.Header.Set("If-Modified-Since", m.LastMod)
	}
}

// Conditional only when out is a complete cache of url: no resume is
// pending, and the meta names the same url and out's current size.
func applyCachedValidator(req *http.Request, out, url string, force bool) {
	if out == "" || force {
		return
	}
	fi, err := os.Stat(out)
	if err != nil {
		return
	}
	if _, err := os.Stat(manifestPath(out)); err == nil {
		return // a resume is pending: not a complete cache
	}
	m, ok := loadMeta(metaPath(out))
	if ok && m.URL == url && m.Size == fi.Size() {
		m.applyConditional(req)
	}
}
