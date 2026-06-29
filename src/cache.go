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
	"encoding/json"
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
	b, err := os.ReadFile(path)
	if err != nil {
		return meta{}, false
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return meta{}, false
	}
	return m, true
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

// save writes m atomically (temp + rename), matching the manifest sidecar so a
// crash mid-write can't leave a torn cache record.
func (m meta) save(path string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

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
