package base

import (
	"os"
	"path/filepath"
	"testing"
)

// prefFiles writes an ini and/or .env in a fresh temp dir, returning their
// paths (a path is still returned when its content is "" and no file written,
// so loadPrefs exercises the missing-file branch).
func prefFiles(tb testing.TB, ini, dotenv string) (iniPath, envPath string) {
	tb.Helper()
	dir := tb.TempDir()
	iniPath = filepath.Join(dir, "config.ini")
	if ini != "" {
		if err := os.WriteFile(iniPath, []byte(ini), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	envPath = filepath.Join(dir, ".env")
	if dotenv != "" {
		if err := os.WriteFile(envPath, []byte(dotenv), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	return iniPath, envPath
}

// TestPrefsPrecedence pins the resolution order the readme advertises:
// real env > .env > ini > default. loadDotEnv mutates the process env, so each
// case clears the key first to stay independent of the ones before it.
func TestPrefsPrecedence(t *testing.T) {
	const key = "PARALLEL"

	t.Run("default when nothing set", func(t *testing.T) {
		os.Unsetenv(key)
		ini, env := prefFiles(t, "", "")
		if got := loadPrefs(ini, env).Parallel; got != 1 {
			t.Fatalf("Parallel = %d, want default 1", got)
		}
	})

	t.Run("ini over default", func(t *testing.T) {
		os.Unsetenv(key)
		ini, env := prefFiles(t, "[pref]\nparallel = 2\n", "")
		if got := loadPrefs(ini, env).Parallel; got != 2 {
			t.Fatalf("Parallel = %d, want ini 2", got)
		}
	})

	t.Run("dotenv over ini", func(t *testing.T) {
		os.Unsetenv(key)
		ini, env := prefFiles(t, "[pref]\nparallel = 2\n", "PARALLEL=3\n")
		if got := loadPrefs(ini, env).Parallel; got != 3 {
			t.Fatalf("Parallel = %d, want .env 3", got)
		}
		os.Unsetenv(key) // loadDotEnv left it set; don't leak into the next case
	})

	t.Run("real env over dotenv", func(t *testing.T) {
		t.Setenv(key, "4") // restored when the subtest ends
		ini, env := prefFiles(t, "[pref]\nparallel = 2\n", "PARALLEL=3\n")
		if got := loadPrefs(ini, env).Parallel; got != 4 {
			t.Fatalf("Parallel = %d, want real env 4", got)
		}
	})
}

// BenchmarkLoadPrefs measures the per-invocation cost of the whole resolution
// path: read .env, read+parse config.ini, and apply env overrides for every
// key. This is the startup tax paid once before any network work.
func BenchmarkLoadPrefs(b *testing.B) {
	ini, env := prefFiles(b,
		"[user]\nuser-agent = gp/1.0\n[pref]\nparallel = 8\nchunk-bytes = 4194304\nretries = 5\nquic = false\n",
		"RETRIES=5\nPARALLEL_MIN=8388608\n",
	)
	b.ReportAllocs()
	for b.Loop() {
		_ = loadPrefs(ini, env)
	}
}

// BenchmarkLoadPrefsNoFiles is the same resolution with neither file present,
// so it isolates the parse + env-lookup work from the file I/O: the gap to
// BenchmarkLoadPrefs is what opening and reading .env + config.ini costs.
func BenchmarkLoadPrefsNoFiles(b *testing.B) {
	dir := b.TempDir()
	ini := filepath.Join(dir, "config.ini") // never created
	env := filepath.Join(dir, ".env")       // never created
	b.ReportAllocs()
	for b.Loop() {
		_ = loadPrefs(ini, env)
	}
}
