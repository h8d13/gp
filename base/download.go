// Parallel range download. Splitting a transfer across N connections only
// helps when the bottleneck is per-connection (CDN throttle, high-BDP
// path); for small bodies the extra handshakes lose, so callers gate on
// size via prefs.ParallelMin before reaching here.
package base

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// rangeable reports whether resp is a length-known 200 that advertised
// byte-range support: the precondition for a parallel or resumable split.
func rangeable(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK &&
		resp.ContentLength > 0 &&
		strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
}

// defaultChunkBytes is the split/resume chunk used when none is configured (it
// matches the config default) and as a guard when a misconfigured 0 would
// otherwise divide the download plan.
const defaultChunkBytes = 4 << 20

// splittable reports whether resp should be fetched as a parallel range split
// under p: more than one connection, a body at or past the split floor, and a
// server that advertised byte ranges. Callers layer their own preconditions
// (an output file, no transfer compression) on top.
func (p prefs) splittable(resp *http.Response) bool {
	return p.Parallel > 1 &&
		resp.ContentLength >= int64(p.ParallelMin) &&
		rangeable(resp)
}

// validatorOf returns the strongest cache validator resp offers: ETag, else
// Last-Modified, else "". An empty validator means resume is unsafe (the
// remote can't be proven unchanged), so the caller forgoes the manifest.
func validatorOf(resp *http.Response) string {
	if e := resp.Header.Get("ETag"); e != "" {
		return e
	}
	return resp.Header.Get("Last-Modified")
}

// splitSegment picks the default per-request byte span for a parallel fetch
// (used only when the user did not pin one with -c). The throughput win of a
// split comes from each connection streaming ONE continuous range: a span much
// smaller than size/conns makes every worker re-issue range requests
// mid-download, and each new request stalls a full RTT (and restarts TCP
// slow-start on the idled socket) before bytes flow. So target size/conns, but
// never go below ChunkBytes (the configured floor) and cap the span so a
// multi-GB file still checkpoints often enough for resume to be worthwhile.
// Because the span depends on conns, a resumed run must reuse the same -p to
// match the manifest; a different -p re-derives the span and restarts
// (manifest.matches gates on chunk).
func splitSegment(size int64, conns int, chunk int64) int64 {
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	if conns < 1 {
		conns = 1
	}
	const maxSeg = 64 << 20
	seg := (size + int64(conns) - 1) / int64(conns)
	if seg < chunk {
		seg = chunk
	}
	if seg > maxSeg {
		seg = maxSeg
	}
	return seg
}

// saveSplitAuto runs an optimal parallel split of resp into out, and is the one
// way both download paths (a URL fetch and `up`) start a split. Unless the
// segment is pinned it auto-sizes the per-request span from the body size and
// connection count, and it always runs on a dedicated h1.1 client so each
// connection gets its own congestion window: an h2/h3 client multiplexes every
// range onto one window and the split would not aggregate (see newSplitClient).
// The caller must have closed the probe body; range requests refetch.
func saveSplitAuto(resp *http.Response, out string, p prefs, pinned bool, rp retryPolicy, prog *progress) (int64, error) {
	if !pinned {
		p.ChunkBytes = int(splitSegment(resp.ContentLength, p.Parallel, int64(p.ChunkBytes)))
	}
	sc, closeSplit := newSplitClient(p)
	defer closeSplit()
	return saveSplit(sc, resp, out, p, rp, prog)
}

// saveSplit downloads resp's URL into out across p.Parallel range requests, each
// spanning p.ChunkBytes (already resolved by saveSplitAuto). When the remote
// offers a validator it persists a resume manifest, reusing any chunks a prior
// run already finished; otherwise it behaves as a plain parallel fetch with no
// on-disk state. Returns bytes fetched THIS run (a resume reports only the gaps).
func saveSplit(client *http.Client, resp *http.Response, out string, p prefs, rp retryPolicy, prog *progress) (int64, error) {
	size := resp.ContentLength
	chunk := int64(p.ChunkBytes)
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	validator := validatorOf(resp)
	mp := manifestPath(out)

	// Reuse prior progress only when the saved manifest still describes this
	// exact remote; any mismatch (or no manifest) means a fresh truncate.
	m, ok := loadManifest(mp)
	resume := ok && m.matches(size, validator, chunk)
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if resume {
		flags = os.O_WRONLY | os.O_CREATE // keep the bytes already on disk
	} else {
		m = newManifest(size, chunk, validator)
	}

	f, err := os.OpenFile(out, flags, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// A resumed run only fetches the gaps, so seed the bar with what already
	// landed; the total stays the full size set by the caller.
	if resume {
		prog.add(int64(len(m.Done)-m.remaining()) * m.Chunk)
	}

	// Persist progress only when a validator lets a later run trust it.
	save := ""
	if validator != "" {
		save = mp
	}
	if resume {
		fmt.Fprintf(os.Stderr, "resume: %d/%d chunks left\n", m.remaining(), len(m.Done))
	}

	n, err := download(client, resp.Request.URL.String(), p.UserAgent, f, m, save, p.Parallel, rp, prog)
	if err != nil {
		return n, err // keep the manifest so the next run continues
	}
	os.Remove(mp) // complete: drop the sidecar (no-op if never written)
	return n, nil
}

// download fetches url into f using up to conns concurrent range requests,
// one chunk of m.Chunk bytes at a time, each written at its offset. Chunks
// m already marks done are skipped; when save is non-empty the manifest is
// flushed after every completed chunk. Returns bytes fetched this run.
func download(client *http.Client, url, ua string, f *os.File, m manifest, save string, conns int, rp retryPolicy, prog *progress) (int64, error) {
	var pending []int
	for i, done := range m.Done {
		if !done {
			pending = append(pending, i)
		}
	}
	if conns > len(pending) {
		conns = len(pending)
	}
	if conns == 0 {
		return 0, nil // nothing left (an already-complete resume)
	}

	var total int64
	var next int64 // atomic cursor into pending
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for {
			k := int(atomic.AddInt64(&next, 1)) - 1
			if k >= len(pending) {
				return
			}
			mu.Lock()
			stop := firstErr != nil
			mu.Unlock()
			if stop { // a sibling failed; stop pulling work
				return
			}

			i := pending[k]
			start := int64(i) * m.Chunk
			end := start + m.Chunk - 1
			if end >= m.Size {
				end = m.Size - 1
			}
			n, err := fetchRange(client, url, ua, f, start, end, rp, prog)
			atomic.AddInt64(&total, n)

			mu.Lock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				m.Done[i] = true
				if save != "" {
					if e := m.save(save); e != nil && firstErr == nil {
						firstErr = e
					}
				}
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}

	for range conns {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return total, firstErr
}

// saveStream copies an already-open response body straight to out,
// truncating any existing file. Used for the small or non-rangeable case
// where splitting and resume don't apply.
func saveStream(out string, body io.Reader, prog *progress) (int64, error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(progWriter{f, prog}, body)
}

// fetchRange GETs bytes [start,end] of url and writes them at offset start
// in f. Concurrent calls at disjoint offsets are safe: WriteAt is pwrite.
func fetchRange(client *http.Client, url, ua string, f *os.File, start, end int64, rp retryPolicy, prog *progress) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	setUserAgent(req, ua)
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range %d-%d: want 206, got %s", start, end, resp.Status)
	}
	return io.Copy(progWriter{io.NewOffsetWriter(f, start), prog}, resp.Body)
}
