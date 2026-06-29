// Conditional-request cache for the download path. After a file lands, gp
// records the remote's validator (ETag and/or Last-Modified) in a .gp-meta
// sidecar beside the -o target. A later run for the same URL replays it as
// If-None-Match / If-Modified-Since, so an unchanged file comes back 304 Not
// Modified and the transfer is skipped entirely. This is the client half of
// HTTP caching -- what curl does only with --etag-compare / -z, here automatic
// and reversible with --force. The conditional request also doubles as the
// size/range probe on a 200, so it adds no round-trip when the file did change.
package src

import (
	"net/http"
	"os"
)

const metaSuffix = ".gp-meta"

// meta is the persisted freshness record for one downloaded file. URL guards
// against replaying one source's validator against a different one written to
// the same path. ETag and LastMod are kept apart because they map to different
// request headers; either alone is enough to ask "still unchanged?".
type meta struct {
	URL     string `json:"url"`
	ETag    string `json:"etag,omitempty"`
	LastMod string `json:"last_modified,omitempty"`
}

func metaPath(out string) string { return out + metaSuffix }

// loadMeta reads a meta sidecar. ok is false when the file is absent or
// unparsable: both mean "no cache", never a hard error.
func loadMeta(path string) (meta, bool) {
	var m meta
	return m, readJSON(path, &m)
}

// metaFrom captures the conditional validators from a response, tagged with the
// URL they describe. ok is false when the server offered neither validator,
// since without one no future conditional request is possible (so there is
// nothing worth saving).
func metaFrom(url string, resp *http.Response) (meta, bool) {
	m := meta{
		URL:     url,
		ETag:    resp.Header.Get("ETag"),
		LastMod: resp.Header.Get("Last-Modified"),
	}
	if m.ETag == "" && m.LastMod == "" {
		return meta{}, false
	}
	return m, true
}

// save writes m atomically beside the -o target (see writeJSONAtomic), so a
// crash mid-write can't leave a torn cache record.
func (m meta) save(path string) error { return writeJSONAtomic(path, m) }

// applyConditional sets the conditional headers on req from a saved meta. Both
// are sent when known; per RFC 9110 the server prefers If-None-Match (the
// strong validator) and falls back to If-Modified-Since.
func (m meta) applyConditional(req *http.Request) {
	if m.ETag != "" {
		req.Header.Set("If-None-Match", m.ETag)
	}
	if m.LastMod != "" {
		req.Header.Set("If-Modified-Since", m.LastMod)
	}
}

// applyCachedValidator makes req conditional when out is already a complete,
// current cache of url: the file exists, no resume is pending (.gp-part
// absent), and the saved meta names this same url. A 304 then skips the
// transfer. force, or any miss, leaves req unconditional for a full fetch.
func applyCachedValidator(req *http.Request, out, url string, force bool) {
	if out == "" || force {
		return
	}
	if _, err := os.Stat(out); err != nil {
		return
	}
	if _, err := os.Stat(manifestPath(out)); err == nil {
		return // a resume is pending: not a complete cache
	}
	if m, ok := loadMeta(metaPath(out)); ok && m.URL == url {
		m.applyConditional(req)
	}
}
