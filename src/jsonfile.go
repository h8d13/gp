// Atomic JSON sidecar read/write, shared by the resume manifest (.gp-part) and
// the cache meta (.gp-meta). Both persist a tiny struct beside the download and
// must survive a crash mid-write, so writes go through a temp file + rename.
package src

import (
	"encoding/json"
	"os"
)

// readJSON loads path into v (a pointer). ok is false when the file is absent
// or unparsable: both mean "no state", never a hard error, so a torn or missing
// sidecar is treated as a fresh start rather than a failure.
func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// writeJSONAtomic marshals v and writes it to path via a temp file + rename, so
// a crash mid-write can never leave a torn sidecar that misreports state.
func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
