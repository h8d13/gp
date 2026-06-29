// Resumable-download bookkeeping. A sidecar manifest next to the -o target
// records which fixed-size chunks already landed, so a killed transfer
// refetches only the gaps instead of restarting. The manifest is keyed on
// (size, validator, chunk): if the remote file changed, none of the stale
// local bytes are reused.
package src

import (
	"encoding/json"
	"os"
)

const manifestSuffix = ".gp-part"

// manifest is the on-disk resume state. Done[i] true means chunk i is fully
// written. Validator is the remote's ETag (or Last-Modified) captured when
// the download began; a mismatch on restart invalidates the whole manifest.
type manifest struct {
	Size      int64  `json:"size"`
	Validator string `json:"validator"`
	Chunk     int64  `json:"chunk"`
	Done      []bool `json:"done"`
}

func manifestPath(out string) string { return out + manifestSuffix }

// newManifest plans size into ceil(size/chunk) not-yet-done chunks.
func newManifest(size, chunk int64, validator string) manifest {
	n := int((size + chunk - 1) / chunk)
	return manifest{Size: size, Validator: validator, Chunk: chunk, Done: make([]bool, n)}
}

// loadManifest reads a manifest from path. ok is false when the file is
// absent or unparsable: both mean "no resume state", not a hard error.
func loadManifest(path string) (manifest, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, false
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return manifest{}, false
	}
	return m, true
}

// matches reports whether a saved manifest still describes the current
// remote: same length, same validator, same chunking. An empty validator
// never matches, so a server that offers no ETag/Last-Modified always
// restarts from zero rather than risk stitching onto a changed file.
func (m manifest) matches(size int64, validator string, chunk int64) bool {
	return validator != "" && m.Size == size &&
		m.Validator == validator && m.Chunk == chunk
}

// remaining counts chunks still to fetch.
func (m manifest) remaining() int {
	n := 0
	for _, d := range m.Done {
		if !d {
			n++
		}
	}
	return n
}

// save writes m atomically (temp + rename in the same dir) so a crash
// mid-write can never leave a torn manifest that misreports progress.
func (m manifest) save(path string) error {
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
