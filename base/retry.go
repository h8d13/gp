// Retry with backoff for rate-limit / transient responses. Only 429 and 503
// retry: they mean "try later". Connection errors are returned as-is so a
// real failure is not masked by silent re-dials.
package base

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"
)

// retryPolicy is the resolved backoff config: max attempts, the first
// backoff step, and the ceiling on any single wait.
type retryPolicy struct {
	n    int
	base time.Duration
	max  time.Duration
}

func (p prefs) retry() retryPolicy {
	return retryPolicy{
		n:    p.Retries,
		base: time.Duration(p.RetryBaseMs) * time.Millisecond,
		max:  time.Duration(p.RetryCapMs) * time.Millisecond,
	}
}

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
// base*2^attempt with full jitter, capped at rp.max.
func nextWait(resp *http.Response, attempt int, rp retryPolicy) (time.Duration, bool) {
	if d, ok := retryAfter(resp); ok {
		return d, true
	}
	d := rp.base << attempt
	if d <= 0 || d > rp.max { // <=0 guards shift overflow at high attempt
		d = rp.max
	}
	return time.Duration(rand.Int64N(int64(d) + 1)), false
}

// doRetry runs req, retrying up to rp.n times on 429/503. A server-set
// Retry-After longer than rp.max stops the loop and surfaces the response
// rather than stalling. req must be replayable (nil-body GET here).
func doRetry(client *http.Client, req *http.Request, rp retryPolicy) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt >= rp.n || !retryStatus(resp.StatusCode) {
			return resp, nil
		}
		wait, fromServer := nextWait(resp, attempt, rp)
		if fromServer && wait > rp.max {
			return resp, nil
		}
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "retry: %s, waiting %v (%d/%d)\n",
			resp.Status, wait.Round(time.Millisecond), attempt+1, rp.n)
		time.Sleep(wait)
	}
}
