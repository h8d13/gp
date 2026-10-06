// Parallel range download. Only helps when the bottleneck is per-connection
// (CDN throttle, high-BDP path); small bodies lose to the extra handshakes,
// hence prefs.ParallelMin.
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

// rangeable is the precondition for a split or resume.
func rangeable(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK &&
		resp.ContentLength > 0 &&
		strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
}

// Also guards a misconfigured 0 from dividing the plan.
const defaultChunkBytes = 4 << 20

// Callers add their own preconditions (output file, no compression).
func (p prefs) splittable(resp *http.Response) bool {
	return p.Parallel > 1 &&
		resp.ContentLength >= int64(p.ParallelMin) &&
		rangeable(resp)
}

// Empty means resume is unsafe: the remote can't be proven unchanged.
func validatorOf(resp *http.Response) string {
	if e := resp.Header.Get("ETag"); e != "" {
		return e
	}
	return resp.Header.Get("Last-Modified")
}

// splitSegment sizes the span when -c is not given. Each worker should
// stream one continuous range: every extra request costs an RTT and restarts
// slow start. So size/conns, floored at chunk, capped so huge files still
// checkpoint for resume. The span depends on conns, so resuming with a
// different -p restarts (manifest.matches gates on chunk).
func splitSegment(size int64, conns int, chunk int64) int64 {
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	conns = max(conns, 1)
	const maxSeg = 64 << 20
	return min(max((size+int64(conns)-1)/int64(conns), chunk), maxSeg)
}

// The caller must have closed the probe body; range requests refetch.
func saveSplitAuto(resp *http.Response, out string, p prefs, pinned bool,
	rp retryPolicy, prog *progress) (int64, error) {
	if !pinned {
		span := splitSegment(resp.ContentLength, p.Parallel, int64(p.ChunkBytes))
		p.ChunkBytes = int(span)
	}
	sc, closeSplit := newSplitClient(p)
	defer closeSplit()
	return saveSplit(sc, resp, out, p, rp, prog)
}

// saveSplit writes a resume manifest only when the remote has a validator.
// Returns bytes fetched this run (a resume reports only the gaps).
func saveSplit(client *http.Client, resp *http.Response, out string, p prefs,
	rp retryPolicy, prog *progress) (int64, error) {
	size := resp.ContentLength
	chunk := int64(p.ChunkBytes)
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	validator := validatorOf(resp)
	mp := manifestPath(out)

	// Reuse prior chunks only if the manifest still describes this remote.
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

	if resume {
		prog.add(m.doneBytes()) // seed the bar with what is on disk
		fmt.Fprintf(os.Stderr, "resume: %d/%d chunks left\n",
			m.remaining(), len(m.Done))
	}
	save := ""
	if validator != "" {
		save = mp
	}

	n, err := download(client, resp.Request.URL.String(), p.UserAgent, f, m,
		save, p.Parallel, rp, prog)
	if err != nil {
		return n, err // keep the manifest so the next run continues
	}
	os.Remove(mp) // complete: drop the sidecar (no-op if never written)
	return n, nil
}

// download skips chunks m marks done; a non-empty save flushes m after
// each chunk.
func download(client *http.Client, url, ua string, f *os.File, m manifest,
	save string, conns int, rp retryPolicy, prog *progress) (int64, error) {
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

	var total, next atomic.Int64 // next: cursor into pending
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for {
			k := int(next.Add(1)) - 1
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
			n, err := fetchRange(client, url, ua, m.Validator, f,
				start, end, rp, prog)
			total.Add(n)

			mu.Lock()
			if err == nil {
				m.Done[i] = true
				if save != "" {
					err = m.save(save)
				}
			}
			if err != nil && firstErr == nil {
				firstErr = err
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
	return total.Load(), firstErr
}

func saveStream(out string, body io.Reader, prog *progress) (int64, error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(progWriter{f, prog}, body)
}

// Concurrent calls at disjoint offsets are safe: WriteAt is pwrite.
func fetchRange(client *http.Client, url, ua, validator string, f *os.File,
	start, end int64, rp retryPolicy, prog *progress) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	// A changed remote then answers 200 instead of splicing in new bytes.
	// RFC 9110 forbids weak ETags and bare dates here, so only strong ETags.
	if strings.HasPrefix(validator, `"`) {
		req.Header.Set("If-Range", validator)
	}
	setUserAgent(req, ua)
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range %d-%d: want 206, got %s",
			start, end, resp.Status)
	}
	// CopyN: a longer body must not spill into the next chunk.
	want := end - start + 1
	w := progWriter{io.NewOffsetWriter(f, start), prog}
	n, err := io.CopyN(w, resp.Body, want)
	if err == io.EOF {
		err = fmt.Errorf("range %d-%d: short body (%d of %d bytes)",
			start, end, n, want)
	}
	return n, err
}
