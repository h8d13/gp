// Black-box e2e: builds the gp binary into out/ (same as build.sh),
// execs it against local httptest servers (plain HTTP and self-signed
// TLS). No network needed.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	root, err := filepath.Abs("..")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(root, "out", "gp")

	build := exec.Command("go", "build", "-o", "out/", ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		panic("build failed: " + string(out))
	}

	os.Exit(m.Run())
}

// run execs the binary in dir with extra env vars, returns combined
// output and exit code. ALLOW_INSECURE is stripped from the inherited
// env so a polluted parent shell can't flip test outcomes.
func run(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "ALLOW_INSECURE=") && !strings.HasPrefix(e, "ALWAYS_ENCRYPT=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	// isolate from any real ~/.config/gp/gpconfig.ini on the dev's box,
	// unless the test supplies its own XDG_CONFIG_HOME
	if !hasEnv(env, "XDG_CONFIG_HOME") {
		cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME="+t.TempDir())
	}
	cmd.Env = append(cmd.Env, env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

func hasEnv(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// xdgWith writes ini to gp/gpconfig.ini under a fresh temp dir and
// returns that dir, for use as XDG_CONFIG_HOME.
func xdgWith(t *testing.T, ini string) string {
	t.Helper()
	dir := t.TempDir()
	gp := filepath.Join(dir, "gp")
	if err := os.MkdirAll(gp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gp, "gpconfig.ini"), []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("hi"))
})

func TestUserAgentFromIni(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.UserAgent()
		w.Write([]byte("hi"))
	}))
	defer srv.Close()

	xdg := xdgWith(t, "[user]\nuser-agent = gp/1.0\n")
	out, code := run(t, t.TempDir(), []string{"XDG_CONFIG_HOME=" + xdg}, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if ua := <-got; ua != "gp/1.0" {
		t.Errorf("User-Agent = %q, want gp/1.0", ua)
	}
}

func TestUserAgentEnvOverridesIni(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.UserAgent()
		w.Write([]byte("hi"))
	}))
	defer srv.Close()

	xdg := xdgWith(t, "[user]\nuser-agent = gp/1.0\n")
	out, code := run(t, t.TempDir(), []string{"XDG_CONFIG_HOME=" + xdg, "USER_AGENT=from-env"}, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if ua := <-got; ua != "from-env" {
		t.Errorf("User-Agent = %q, want from-env", ua)
	}
}

func TestPlainHTTP(t *testing.T) {
	srv := httptest.NewServer(okHandler)
	defer srv.Close()

	out, code := run(t, t.TempDir(), nil, srv.URL)
	if code != 0 || !strings.Contains(out, "200 OK") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestSchemeDefaultsToHTTPS(t *testing.T) {
	srv := httptest.NewServer(okHandler)
	defer srv.Close()

	// TLS against a plain server fails, but the error proves https:// was prefixed
	hostport := strings.TrimPrefix(srv.URL, "http://")
	out, code := run(t, t.TempDir(), nil, hostport)
	if code == 0 || !strings.Contains(out, "https://"+hostport) {
		t.Errorf("exit=%d out=%q, scheme-less should default to https", code, out)
	}
}

func TestAlwaysEncryptOffIni(t *testing.T) {
	srv := httptest.NewServer(okHandler)
	defer srv.Close()

	xdg := xdgWith(t, "[pref]\nalways-encrypt = false\n")
	hostport := strings.TrimPrefix(srv.URL, "http://")
	out, code := run(t, t.TempDir(), []string{"XDG_CONFIG_HOME=" + xdg}, hostport)
	if code != 0 || !strings.Contains(out, "http://"+hostport) {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestAlwaysEncryptOffEnv(t *testing.T) {
	srv := httptest.NewServer(okHandler)
	defer srv.Close()

	hostport := strings.TrimPrefix(srv.URL, "http://")
	out, code := run(t, t.TempDir(), []string{"ALWAYS_ENCRYPT=0"}, hostport)
	if code != 0 || !strings.Contains(out, "http://"+hostport) {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestOutputFlagSavesBody(t *testing.T) {
	srv := httptest.NewServer(okHandler)
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "nested", "dir", "body.txt")
	out, code := run(t, dir, nil, "-o", dest, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Errorf("saved body = %q, want hi", got)
	}
}

func TestTLSRejectedByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(okHandler)
	defer srv.Close()

	out, code := run(t, t.TempDir(), nil, srv.URL)
	if code == 0 {
		t.Errorf("self-signed cert should fail, out=%q", out)
	}
	if !strings.Contains(out, "certificate") {
		t.Errorf("want cert error, out=%q", out)
	}
}

func TestEnvVarAllowsInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(okHandler)
	defer srv.Close()

	out, code := run(t, t.TempDir(), []string{"ALLOW_INSECURE=true"}, srv.URL)
	if code != 0 || !strings.Contains(out, "200 OK") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestIniAllowsInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(okHandler)
	defer srv.Close()

	xdg := xdgWith(t, "[pref]\n# comment line\nallow-insecure = true\n")
	out, code := run(t, t.TempDir(), []string{"XDG_CONFIG_HOME=" + xdg}, srv.URL)
	if code != 0 || !strings.Contains(out, "200 OK") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestDotEnvAllowsInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(okHandler)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ALLOW_INSECURE=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, dir, nil, srv.URL)
	if code != 0 || !strings.Contains(out, "200 OK") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

// rangeServer serves a fixed body and honors "bytes=lo-hi" requests. Its
// barrier holds each range request until `expect` of them coexist, turning
// "did gp open N connections" into a deterministic check, not a timing race.
type rangeServer struct {
	body    []byte
	expect  int
	mu      sync.Mutex
	cond    *sync.Cond
	cur     int
	max     int
	arrived int
}

func newRangeServer(body []byte, expect int) *rangeServer {
	s := &rangeServer{body: body, expect: expect}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *rangeServer) maxConc() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

func (s *rangeServer) enter() {
	s.mu.Lock()
	s.cur++
	if s.cur > s.max {
		s.max = s.cur
	}
	s.mu.Unlock()
}

func (s *rangeServer) leave() {
	s.mu.Lock()
	s.cur--
	s.mu.Unlock()
}

// barrier blocks until expect range requests have arrived together.
func (s *rangeServer) barrier() {
	s.mu.Lock()
	s.arrived++
	if s.arrived >= s.expect {
		s.cond.Broadcast()
	}
	for s.arrived < s.expect {
		s.cond.Wait()
	}
	s.mu.Unlock()
}

func (s *rangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.enter()
	defer s.leave()

	w.Header().Set("Accept-Ranges", "bytes")
	lo, hi := 0, len(s.body)-1
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &lo, &hi); err != nil || lo < 0 || hi >= len(s.body) || lo > hi {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(s.body)))
		status = http.StatusPartialContent
		s.barrier() // prove all N range requests coexist
	}
	w.Header().Set("Content-Length", strconv.Itoa(hi-lo+1))
	w.WriteHeader(status)
	w.Write(s.body[lo : hi+1])
}

// patternBytes fills n bytes with a non-repeating LCG stream so a chunk
// written to the wrong offset is guaranteed to mismatch.
func patternBytes(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x9e3779b9)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func gpEnvWith(t *testing.T, kv ...string) []string {
	return append([]string{"XDG_CONFIG_HOME=" + t.TempDir()}, kv...)
}

func TestParallelDownloadSplitsAndReassembles(t *testing.T) {
	const conns = 4
	body := patternBytes(256 << 10)
	rs := newRangeServer(body, conns)
	srv := httptest.NewServer(rs)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out.bin")
	// chunk = body/conns so the split is exactly `conns` concurrent ranges.
	chunk := strconv.Itoa(len(body) / conns)
	env := gpEnvWith(t, "PARALLEL="+strconv.Itoa(conns), "PARALLEL_MIN=1", "CHUNK_BYTES="+chunk)
	out, code := run(t, t.TempDir(), env, "-o", dst, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("reassembled body differs: got %d bytes, want %d", len(got), len(body))
	}
	if mc := rs.maxConc(); mc < conns {
		t.Errorf("maxConc=%d, want >=%d (did not split into parallel connections)", mc, conns)
	}
}

func TestSmallBodyStaysSingleStream(t *testing.T) {
	body := patternBytes(4096)
	rs := newRangeServer(body, 4)
	srv := httptest.NewServer(rs)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out.bin")
	// threshold above the body size: must not split
	env := gpEnvWith(t, "PARALLEL=4", "PARALLEL_MIN=1048576")
	out, code := run(t, t.TempDir(), env, "-o", dst, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}

	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, body) {
		t.Fatalf("body differs: got %d, want %d", len(got), len(body))
	}
	if mc := rs.maxConc(); mc != 1 {
		t.Errorf("maxConc=%d, want 1 (small body must not split)", mc)
	}
}

func TestParallelDisabledStaysSingleStream(t *testing.T) {
	body := patternBytes(256 << 10)
	rs := newRangeServer(body, 2)
	srv := httptest.NewServer(rs)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out.bin")
	// big body + ranges available, but parallel turned off
	env := gpEnvWith(t, "PARALLEL=1", "PARALLEL_MIN=1")
	out, code := run(t, t.TempDir(), env, "-o", dst, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}

	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, body) {
		t.Fatalf("body differs: got %d, want %d", len(got), len(body))
	}
	if mc := rs.maxConc(); mc != 1 {
		t.Errorf("maxConc=%d, want 1 (PARALLEL=1 disables splitting)", mc)
	}
}

// etagRangeServer serves body with an ETag and records every Range header it
// receives, so a test can assert exactly which chunks were (re)fetched. The
// initial no-Range probe returns 200 and is not recorded.
type etagRangeServer struct {
	body   []byte
	etag   string
	mu     sync.Mutex
	ranges []string
}

func (s *etagRangeServer) gotRanges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *etagRangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", s.etag)
	lo, hi := 0, len(s.body)-1
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		fmt.Sscanf(rng, "bytes=%d-%d", &lo, &hi)
		s.mu.Lock()
		s.ranges = append(s.ranges, rng)
		s.mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(s.body)))
		status = http.StatusPartialContent
	}
	w.Header().Set("Content-Length", strconv.Itoa(hi-lo+1))
	w.WriteHeader(status)
	w.Write(s.body[lo : hi+1])
}

// writeManifest hand-writes a .gp-part sidecar in gp's on-disk shape. The
// e2e suite is black-box, so it can't import the manifest type; the JSON
// field names here mirror its json tags.
func writeManifest(t *testing.T, path string, size, chunk int64, validator string, done []bool) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"size": size, "validator": validator, "chunk": chunk, "done": done,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRefetchesOnlyMissingChunks(t *testing.T) {
	const chunk = 64 << 10
	body := patternBytes(4 * chunk) // chunks 0..3
	rs := &etagRangeServer{body: body, etag: `"v1"`}
	srv := httptest.NewServer(rs)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")

	// Pre-seed: chunks 0 and 2 already on disk and marked done; 1 and 3 gap.
	seed := make([]byte, len(body))
	copy(seed[0:chunk], body[0:chunk])
	copy(seed[2*chunk:3*chunk], body[2*chunk:3*chunk])
	if err := os.WriteFile(dst, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dst+".gp-part", int64(len(body)), chunk, `"v1"`,
		[]bool{true, false, true, false})

	env := gpEnvWith(t, "PARALLEL=2", "PARALLEL_MIN=1", "CHUNK_BYTES="+strconv.Itoa(chunk))
	out, code := run(t, dir, env, "-o", dst, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("resumed file differs: got %d bytes, want %d", len(got), len(body))
	}

	want := map[string]bool{
		fmt.Sprintf("bytes=%d-%d", chunk, 2*chunk-1):   true, // chunk 1
		fmt.Sprintf("bytes=%d-%d", 3*chunk, 4*chunk-1): true, // chunk 3
	}
	ranges := rs.gotRanges()
	if len(ranges) != len(want) {
		t.Fatalf("ranges=%v, want only the 2 missing chunks", ranges)
	}
	for _, r := range ranges {
		if !want[r] {
			t.Errorf("refetched unexpected range %s (only chunks 1,3 should refetch)", r)
		}
	}

	if _, err := os.Stat(dst + ".gp-part"); !os.IsNotExist(err) {
		t.Errorf("manifest not removed after completion: err=%v", err)
	}
}

func TestResumeRestartsWhenValidatorChanged(t *testing.T) {
	const chunk = 64 << 10
	body := patternBytes(4 * chunk)
	rs := &etagRangeServer{body: body, etag: `"v2"`}
	srv := httptest.NewServer(rs)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")

	// Stale local file + manifest from a DIFFERENT remote version (v1).
	garbage := bytes.Repeat([]byte{0xAA}, len(body))
	if err := os.WriteFile(dst, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dst+".gp-part", int64(len(body)), chunk, `"v1"`,
		[]bool{true, true, true, true})

	env := gpEnvWith(t, "PARALLEL=4", "PARALLEL_MIN=1", "CHUNK_BYTES="+strconv.Itoa(chunk))
	out, code := run(t, dir, env, "-o", dst, srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("stale bytes not replaced after validator change")
	}
	if n := len(rs.gotRanges()); n != 4 {
		t.Errorf("refetched %d chunks, want all 4 (validator changed)", n)
	}
}
