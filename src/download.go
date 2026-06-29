// Parallel range download. Splitting a transfer across N connections only
// helps when the bottleneck is per-connection (CDN throttle, high-BDP
// path); for small bodies the extra handshakes lose, so callers gate on
// size via prefs.ParallelMin before reaching here.
package src

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

// validatorOf returns the strongest cache validator resp offers: ETag, else
// Last-Modified, else "". An empty validator means resume is unsafe (the
// remote can't be proven unchanged), so the caller forgoes the manifest.
func validatorOf(resp *http.Response) string {
	if e := resp.Header.Get("ETag"); e != "" {
		return e
	}
	return resp.Header.Get("Last-Modified")
}

// saveSplit downloads resp's URL into out across p.Parallel range requests,
// chunked at p.ChunkBytes. When the remote offers a validator it persists a
// resume manifest, reusing any chunks a prior run already finished; otherwise
// it behaves as a plain parallel fetch with no on-disk state. The probe body
// is assumed already drained/closed by the caller (range requests refetch).
// Returns bytes fetched THIS run (a resumed run reports only the gaps).
func saveSplit(client *http.Client, resp *http.Response, out string, p prefs, rp retryPolicy, prog *progress) (int64, error) {
	size := resp.ContentLength
	chunk := int64(p.ChunkBytes)
	if chunk <= 0 {
		chunk = 4 << 20 // guard a misconfigured 0 from dividing the plan
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
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
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
