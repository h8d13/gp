// The `up` subcommand: keep tools from GitHub releases current. A sources.ini
// next to gpconfig.ini lists [NAME] sections (repo/match/ext/dest); `gp up`
// resolves each to the latest release, and downloads+installs only when the
// release tag differs from what a lockfile records as installed. Tag equality
// is the version check, so no per-tool version parsing is needed.
package src

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// source is one [NAME] section of sources.ini.
type source struct {
	name  string   // the section name, e.g. CODIUM
	forge string   // github (default) | gitea | gitlab
	host  string   // API host; "" uses the forge default (self-host override)
	repo  string   // owner/repo (project path) on the forge
	match []string // substrings the asset name must all contain
	ext   string   // extension the asset name must end with
	dest  string   // where to install (extract dir, or file dir for non-tar)
}

// sourcesPath and lockPath live beside gpconfig.ini so all gp state is in one
// place under $XDG_CONFIG_HOME/gp.
func sourcesPath() string { return filepath.Join(filepath.Dir(configPath()), "sources.ini") }
func lockPath() string    { return filepath.Join(filepath.Dir(configPath()), "sources.lock") }

// loadSources parses sources.ini into sources sorted by name (deterministic
// output and a stable "all" order). Each section needs repo and dest; ext and
// match are optional (no ext means match by substring only).
func loadSources(path string) ([]source, error) {
	cfg := loadConfig(path)
	if len(cfg) == 0 {
		return nil, fmt.Errorf("no sources in %s", path)
	}
	var names []string
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)

	var srcs []source
	for _, name := range names {
		sec := cfg[name]
		s := source{
			name:  name,
			forge: sec["forge"],
			host:  sec["host"],
			repo:  sec["repo"],
			ext:   sec["ext"],
			dest:  expandHome(sec["dest"]),
		}
		if f := strings.Fields(sec["match"]); len(f) > 0 {
			s.match = f
		}
		if s.repo == "" || s.dest == "" {
			return nil, fmt.Errorf("[%s]: repo and dest are required", name)
		}
		if _, err := forgeFor(s.forge); err != nil {
			return nil, fmt.Errorf("[%s]: %w", name, err)
		}
		srcs = append(srcs, s)
	}
	return srcs, nil
}

// expandHome turns a leading ~ into the user's home dir; other paths are
// returned unchanged.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// upMain is the `gp up [NAME...]` entry point. With no names it processes every
// source; otherwise only the named ones. It exits non-zero if any source fails,
// but still attempts the rest so one bad entry does not block the others.
func upMain(names []string, p prefs) {
	srcs, err := loadSources(sourcesPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "up:", err)
		os.Exit(1)
	}
	if len(names) > 0 {
		srcs = selectSources(srcs, names)
	}

	lock := loadConfig(lockPath()) // [installed] NAME = tag
	installed := lock["installed"]
	if installed == nil {
		installed = map[string]string{}
	}

	client, closeClient := newClient(p)
	defer closeClient()
	rp := p.retry()

	failed := false
	for _, s := range srcs {
		newTag, err := syncOne(client, s, installed[s.name], p, rp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", s.name, err)
			failed = true
			continue
		}
		if newTag != "" {
			installed[s.name] = newTag
			if err := saveLock(lockPath(), installed); err != nil {
				fmt.Fprintln(os.Stderr, "up: write lock:", err)
				failed = true
			}
		}
	}
	if failed {
		os.Exit(1)
	}
}

// selectSources filters srcs to the requested names, warning about any name
// with no matching section.
func selectSources(srcs []source, names []string) []source {
	byName := map[string]source{}
	for _, s := range srcs {
		byName[s.name] = s
	}
	var out []source
	for _, n := range names {
		if s, ok := byName[n]; ok {
			out = append(out, s)
		} else {
			fmt.Fprintf(os.Stderr, "up: no source named %q\n", n)
		}
	}
	return out
}

// syncOne resolves s to its latest release and installs it when the tag differs
// from have (the recorded installed tag) or the destination is missing. It
// returns the newly installed tag, or "" when nothing changed.
func syncOne(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	f, err := forgeFor(s.forge) // already validated in loadSources
	if err != nil {
		return "", err
	}
	rel, err := fetchLatestRelease(client, f, s.host, s.repo, p.UserAgent, rp)
	if err != nil {
		return "", err
	}
	if rel.TagName == have && destPopulated(s.dest) {
		fmt.Printf("%s: up to date (%s)\n", s.name, rel.TagName)
		return "", nil
	}
	asset, err := rel.pickAsset(s.match, s.ext)
	if err != nil {
		return "", err
	}
	if have == "" {
		fmt.Printf("%s: installing %s (%s)\n", s.name, rel.TagName, asset.Name)
	} else {
		fmt.Printf("%s: %s -> %s (%s)\n", s.name, have, rel.TagName, asset.Name)
	}

	if err := install(client, asset, s, p, rp); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// install downloads asset and places it at s.dest: tar/tar.gz/tgz archives are
// extracted into the directory; anything else (AppImage, .deb, a bare binary)
// is saved as a file in it. The archive is staged in a temp file and removed
// afterwards, mirroring the main download path's extract-only handling.
func install(client *http.Client, a asset, s source, p prefs, rp retryPolicy) error {
	if isTarball(a.Name) {
		tmp, err := os.CreateTemp("", "gp-up-*")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())

		if _, err := fetchToFile(client, a.URL, tmp.Name(), p, rp, "DL"); err != nil {
			return err
		}
		if fi, e := os.Stat(tmp.Name()); e == nil {
			prog := newProgress(fi.Size(), "XT", p.Progress)
			prog.run()
			_, err = extractTarGz(tmp.Name(), s.dest, prog)
			prog.finish()
		}
		return err
	}

	// Non-archive asset: drop it into dest under its own name.
	if err := os.MkdirAll(s.dest, 0o755); err != nil {
		return err
	}
	_, err := fetchToFile(client, a.URL, filepath.Join(s.dest, a.Name), p, rp, "DL")
	return err
}

// fetchToFile streams url to out with a progress bar, following redirects (the
// CDN hop browser_download_url makes). It does not split: the redirected target
// is not assumed rangeable, and release assets fit a single stream fine.
func fetchToFile(client *http.Client, url, out string, p prefs, rp retryPolicy, label string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return 0, err
	}
	prog := newProgress(resp.ContentLength, label, p.Progress)
	prog.run()
	n, err := saveStream(out, resp.Body, prog)
	prog.finish()
	return n, err
}

// isTarball reports whether name is a tar archive gp can extract.
func isTarball(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".tar") || strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tgz")
}

// destPopulated reports whether dest exists and is non-empty, so a matching
// lock tag with a deleted install still triggers a re-fetch.
func destPopulated(dest string) bool {
	fi, err := os.Stat(dest)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return fi.Size() > 0
	}
	entries, err := os.ReadDir(dest)
	return err == nil && len(entries) > 0
}

// saveLock writes the installed-tag map back as an ini [installed] section,
// keys sorted for a stable file. Reuses loadConfig's format so the next run
// reads it straight back.
func saveLock(path string, installed map[string]string) error {
	var keys []string
	for k := range installed {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# written by gp up; tracks the installed release tag per source\n")
	b.WriteString("[installed]\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, installed[k])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
