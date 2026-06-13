// Retry with backoff for rate-limit / transient responses. Only 429 and 503
// retry: they mean "try later". Connection errors are returned as-is so a
// real failure is not masked by silent re-dials.
package main

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"
)

const capWait = 30 * time.Second

// retryStatus reports whether a response code warrants waiting and retrying.
func retryStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// retryAfter parses a Retry-After header in either form: delta-seconds or an
// HTTP-date. Returns the delay and whether the header was present and valid.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// nextWait returns the delay before the next attempt and whether the server
// dictated it. A server Retry-After is taken as-is; otherwise exponential
// 500ms*2^attempt with full jitter, capped at capWait.
func nextWait(resp *http.Response, attempt int) (time.Duration, bool) {
	if d, ok := retryAfter(resp); ok {
		return d, true
	}
	d := time.Duration(1<<attempt) * 500 * time.Millisecond
	if d > capWait {
		d = capWait
	}
	return time.Duration(rand.Int64N(int64(d) + 1)), false
}

// doRetry runs req, retrying up to retries times on 429/503. A server-set
// Retry-After longer than capWait stops the loop and surfaces the response
// rather than stalling. req must be replayable (nil-body GET here).
func doRetry(client *http.Client, req *http.Request, retries int) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt >= retries || !retryStatus(resp.StatusCode) {
			return resp, nil
		}
		wait, fromServer := nextWait(resp, attempt)
		if fromServer && wait > capWait {
			return resp, nil
		}
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "retry: %s, waiting %v (%d/%d)\n",
			resp.Status, wait.Round(time.Millisecond), attempt+1, retries)
		time.Sleep(wait)
	}
}
