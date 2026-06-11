// Config and env override logic. Every ini key has an env var
// equivalent (allow-insecure -> ALLOW_INSECURE); env wins.
package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// prefs is the typed view of [pref]: the single place where keys,
// defaults, and their meaning live. Extend here when adding a key.
type prefs struct {
	AllowInsecure bool // skip TLS cert verification (default false)
	AlwaysEncrypt bool // scheme-less URLs get https:// (default true)
}

// loadPrefs reads .env then ini and resolves all [pref] keys.
// Precedence: real env > .env > ini > default.
func loadPrefs(iniPath, envPath string) prefs {
	loadDotEnv(envPath)
	cfg := loadConfig(iniPath)
	return prefs{
		AllowInsecure: cfg.boolOr("pref", "allow-insecure", false),
		AlwaysEncrypt: cfg.boolOr("pref", "always-encrypt", true),
	}
}

// config holds ini sections as section -> key -> value.
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
