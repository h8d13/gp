// Resume bookkeeping. A .gp-part manifest records which chunks landed, so a
// killed transfer refetches only the gaps. Keyed on (size, validator,
// chunk): a changed remote reuses none of the stale bytes.
package base

const manifestSuffix = ".gp-part"

type manifest struct {
	Size      int64  `json:"size"`
	Validator string `json:"validator"`
	Chunk     int64  `json:"chunk"`
	Done      []bool `json:"done"`
}

func manifestPath(out string) string { return out + manifestSuffix }

func newManifest(size, chunk int64, validator string) manifest {
	n := int((size + chunk - 1) / chunk)
	return manifest{Size: size, Validator: validator, Chunk: chunk,
		Done: make([]bool, n)}
}

func loadManifest(path string) (manifest, bool) {
	var m manifest
	return m, readJSON(path, &m)
}

// An empty validator never matches: the remote can't be proven unchanged.
func (m manifest) matches(size int64, validator string, chunk int64) bool {
	return validator != "" && m.Size == size &&
		m.Validator == validator && m.Chunk == chunk
}

// doneBytes is exact: the last chunk may be short.
func (m manifest) doneBytes() int64 {
	var n int64
	for i, d := range m.Done {
		if d {
			n += min(m.Chunk, m.Size-int64(i)*m.Chunk)
		}
	}
	return n
}

func (m manifest) remaining() int {
	n := 0
	for _, d := range m.Done {
		if !d {
			n++
		}
	}
	return n
}

func (m manifest) save(path string) error { return writeJSONAtomic(path, m) }
