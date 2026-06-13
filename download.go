// Parallel range download. Splitting a transfer across N connections only
// helps when the bottleneck is per-connection (CDN throttle, high-BDP
// path); for small bodies the extra handshakes lose, so callers gate on
// size via prefs.ParallelMin before reaching here.
package main

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
// byte-range support: the precondition for a parallel split.
func rangeable(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK &&
		resp.ContentLength > 0 &&
		strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
}

// parallelDownload fetches url into f over conns byte-range requests, each
// writing its slice at the correct offset. Returns total bytes written.
// Assumes size > 0 and that the server honors ranges (see rangeable).
func parallelDownload(client *http.Client, url, ua string, f *os.File, size int64, conns, retries int) (int64, error) {
	chunk := size / int64(conns)
	var total int64
	errs := make([]error, conns)
	var wg sync.WaitGroup

	for i := range conns {
		start := int64(i) * chunk
		end := start + chunk - 1
		if i == conns-1 {
			end = size - 1 // last part absorbs the division remainder
		}
		wg.Add(1)
		go func(i int, start, end int64) {
			defer wg.Done()
			n, err := fetchRange(client, url, ua, f, start, end, retries)
			atomic.AddInt64(&total, n)
			errs[i] = err
		}(i, start, end)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// fetchRange GETs bytes [start,end] of url and writes them at offset start
// in f. Concurrent calls at disjoint offsets are safe: WriteAt is pwrite.
func fetchRange(client *http.Client, url, ua string, f *os.File, start, end int64, retries int) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := doRetry(client, req, retries)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range %d-%d: want 206, got %s", start, end, resp.Status)
	}
	return io.Copy(io.NewOffsetWriter(f, start), resp.Body)
}
