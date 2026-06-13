// Config and env override logic. Every ini key has an env var
// equivalent (allow-insecure -> ALLOW_INSECURE); env wins.
package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// configPath resolves the ini location per the XDG Base Directory spec:
// $XDG_CONFIG_HOME/gp/gpconfig.ini, else ~/.config/gp/gpconfig.ini.
// XDG_CONFIG_HOME is honored only when absolute (spec requirement);
// a relative or unset value falls back to ~/.config.
func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gp", "gpconfig.ini")
}

// prefs is the typed view of [pref]: the single place where keys,
// defaults, and their meaning live. Extend here when adding a key.
type prefs struct {
	AllowInsecure bool   // skip TLS cert verification (default false)
	AlwaysEncrypt bool   // scheme-less URLs get https:// (default true)
	UserAgent     string // User-Agent header; "" leaves Go's default
	Parallel      int    // range-download connections; 1 (default) disables
	ParallelMin   int    // min body size in bytes to split (default 8 MiB)
	Quic          bool   // speak HTTP/3 over QUIC instead of h1/h2 (default false)
	Retries       int    // retries on 429/503 with backoff (default 3)
	RetryBaseMs   int    // first backoff step in ms when no Retry-After (default 500)
	RetryCapMs    int    // ceiling on any single backoff wait in ms (default 30000)
}

// loadPrefs reads .env then ini and resolves all [pref] keys.
// Precedence: real env > .env > ini > default.
func loadPrefs(iniPath, envPath string) prefs {
	loadDotEnv(envPath)
	cfg := loadConfig(iniPath)
	return prefs{
		AllowInsecure: cfg.boolOr("pref", "allow-insecure", false),
		AlwaysEncrypt: cfg.boolOr("pref", "always-encrypt", true),
		UserAgent:     cfg.get("user", "user-agent"),
		Parallel:      cfg.intOr("pref", "parallel", 1),
		ParallelMin:   cfg.intOr("pref", "parallel-min", 8<<20),
		Quic:          cfg.boolOr("pref", "quic", false),
		Retries:       cfg.intOr("pref", "retries", 3),
		RetryBaseMs:   cfg.intOr("pref", "retry-base-ms", 500),
		RetryCapMs:    cfg.intOr("pref", "retry-cap-ms", 30000),
	}
}

type config map[string]map[string]string

// loadConfig parses the ini file once. Missing file means empty config.
func loadConfig(path string) config {
	cfg := config{}
	f, err := os.Open(path)
	if err != nil {
		return cfg
	}
	defer f.Close()

	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			if cfg[section] == nil {
				cfg[section] = map[string]string{}
			}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && section != "" {
			cfg[section][strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return cfg
}

// envKey maps an ini key to its env var: allow-insecure -> ALLOW_INSECURE.
func envKey(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

// get returns section/key with env var override taking precedence.
func (c config) get(section, key string) string {
	if v, ok := os.LookupEnv(envKey(key)); ok {
		return v
	}
	return c[section][key]
}

// boolOr parses section/key as bool (true/false/1/0/t/f, any case),
// falling back to def when missing or malformed.
func (c config) boolOr(section, key string, def bool) bool {
	if v, err := strconv.ParseBool(c.get(section, key)); err == nil {
		return v
	}
	return def
}

// intOr parses section/key as an int, falling back to def when missing
// or malformed.
func (c config) intOr(section, key string, def int) int {
	if v, err := strconv.Atoi(c.get(section, key)); err == nil {
		return v
	}
	return def
}

// loadDotEnv sets vars from a .env file. Already-set env always wins,
// so real environment overrides .env, which overrides ini.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
