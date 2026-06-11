// Black-box e2e: builds the gp binary into out/ (same as build.sh),
// execs it against local httptest servers (plain HTTP and self-signed
// TLS). No network needed.
package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("hi"))
})

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

	dir := t.TempDir()
	ini := "[pref]\nalways-encrypt = false\n"
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	hostport := strings.TrimPrefix(srv.URL, "http://")
	out, code := run(t, dir, nil, hostport)
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

	dir := t.TempDir()
	ini := "[pref]\n# comment line\nallow-insecure = true\n"
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, dir, nil, srv.URL)
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
