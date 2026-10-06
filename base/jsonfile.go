// JSON sidecars (.gp-part, .gp-meta). A missing or unparsable file reads as
// "no state", never an error; writes go temp + rename so a crash can't
// leave a torn file.
package base

import (
	"encoding/json"
	"os"
)

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

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
