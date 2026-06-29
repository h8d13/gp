// The `up` subcommand: keep tools from GitHub releases current. A sources.ini
// next to config.ini lists [NAME] sections (repo/match/ext/dest); `gp up`
// resolves each to the latest release, and downloads+installs only when the
// release tag differs from what a lockfile records as installed. Tag equality
// is the version check, so no per-tool version parsing is needed.
package src

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// source is one [NAME] section of sources.ini. A source is either a forge
// release (repo + match/ext on a forge) or a bare url (a direct file link);
// url, when set, takes over and the forge fields are ignored.
type source struct {
	name    string   // the section name, e.g. CODIUM
	url     string   // direct file URL; when set this is a bare-url source
	index   string   // directory-index URL; pick a file from its links by match/ext
	forge   string   // github (default) | gitea | gitlab
	host    string   // API host; "" uses the forge default (self-host override)
	repo    string   // owner/repo (project path) on the forge
	match   []string // substrings the asset name must all contain
	ext     string   // extension the asset name must end with
	as      string   // rename a single-file install to this name (archives ignore it)
	dest    string   // where to install (extract dir, or file dir for non-tar)
	extract bool     // force tar extraction even when the name lacks an archive ext
}

// sourcesPath and lockPath live beside config.ini so all gp state is in one
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
			url:   sec["url"],
			index: sec["index"],
			forge: sec["forge"],
			host:  sec["host"],
			repo:  sec["repo"],
			ext:   sec["ext"],
			as:    sec["as"],
			dest:  expandHome(sec["dest"]),
		}
		if f := strings.Fields(sec["match"]); len(f) > 0 {
			s.match = f
		}
		s.extract, _ = strconv.ParseBool(sec["extract"]) // "" / bad -> false
		if s.dest == "" {
			return nil, fmt.Errorf("[%s]: dest is required", name)
		}
		if strings.ContainsRune(s.as, '/') {
			return nil, fmt.Errorf("[%s]: as must be a bare filename, not a path", name)
		}
		// Resolve the source type, in precedence order: a bare url is the file
		// itself; an index is a listing to pick one file from; otherwise it is
		// a forge release. Each later field is ignored once an earlier one wins.
		switch {
		case s.url != "":
		case s.index != "":
			if len(s.match) == 0 && s.ext == "" {
				return nil, fmt.Errorf("[%s]: index needs match or ext to pick a file", name)
			}
		case s.repo != "":
			if _, err := forgeFor(s.forge); err != nil {
				return nil, fmt.Errorf("[%s]: %w", name, err)
			}
		default:
			return nil, fmt.Errorf("[%s]: url, index, or repo is required", name)
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

// upMain is the `gp up` entry point: it processes every source in sources.ini.
// It exits non-zero if any source fails, but still attempts the rest so one bad
// entry does not block the others.
func upMain(p prefs) {
	srcs, err := loadSources(sourcesPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "up:", err)
		os.Exit(1)
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

// syncOne resolves s to its latest release and installs it when the tag differs
// from have (the recorded installed tag) or the destination is missing. It
// returns the newly installed tag, or "" when nothing changed.
func syncOne(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	if s.url != "" {
		return syncURL(client, s, have, p, rp)
	}
	if s.index != "" {
		return syncIndex(client, s, have, p, rp)
	}
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

// syncURL handles a bare-url source: the URL is the file, installed under its
// own basename (or `as`). Versioning is the HTTP validator (see installFile).
func syncURL(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	return installFile(client, s, destName(s, urlBase(s.url)), s.url, have, p, rp)
}

// syncIndex handles an index source: fetch the listing, pick the single file
// matching match/ext, then install it like a bare url (validator-versioned).
// A listing that keeps multiple matching files (e.g. several dated builds) is
// an ambiguous pick and errors with the candidates, so point an index source
// at a "latest"-style directory or tighten match/ext.
func syncIndex(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	rel, err := fetchIndex(client, s.index, p.UserAgent, rp)
	if err != nil {
		return "", err
	}
	a, err := rel.pickAsset(s.match, s.ext)
	if err != nil {
		return "", err
	}
	return installFile(client, s, destName(s, a.Name), a.URL, have, p, rp)
}

// installFile installs the concrete file at fileURL for a source whose version
// is the HTTP validator (ETag, else Last-Modified) rather than a release tag:
// a HEAD reads it, and when it still matches the lock and the dest is populated
// nothing is refetched. The file is placed like any asset (tarball extracted,
// anything else saved under name). Returns the validator to record, or "" when
// the server offers none (then every run reinstalls). Shared by the bare-url
// and index sources, which differ only in how they arrive at fileURL.
func installFile(client *http.Client, s source, name, fileURL, have string, p prefs, rp retryPolicy) (string, error) {
	validator := headValidator(client, fileURL, p.UserAgent, rp)
	if validator != "" && validator == have && destPopulated(s.dest) {
		fmt.Printf("%s: up to date\n", s.name)
		return "", nil
	}
	if have == "" {
		fmt.Printf("%s: installing %s\n", s.name, name)
	} else {
		fmt.Printf("%s: updating %s\n", s.name, name)
	}
	if err := install(client, asset{Name: name, URL: fileURL}, s, p, rp); err != nil {
		return "", err
	}
	return validator, nil
}

// headValidator returns the URL's ETag (else Last-Modified) via a HEAD request,
// or "" when the request fails or the server offers neither. It is advisory: a
// "" only forces a reinstall, never an error.
func headValidator(client *http.Client, rawURL, ua string, rp retryPolicy) string {
	req, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		return ""
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return validatorOf(resp)
}

// urlBase derives a filename from a URL's last path segment, falling back to
// "download" when the URL carries no usable name.
func urlBase(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if b := path.Base(u.Path); b != "" && b != "." && b != "/" {
			return b
		}
	}
	return "download"
}

// install downloads asset and places it at s.dest: tar archives are extracted
// into the directory; anything else (AppImage, .deb, a bare binary) is saved as
// a file in it. Extraction fires when the name carries a known archive
// extension, or when the source sets extract=true (for archive URLs that end in
// no usable name, e.g. GitHub's /tarball API). The archive is staged in a temp
// file and removed afterwards, mirroring the main download path.
func install(client *http.Client, a asset, s source, p prefs, rp retryPolicy) error {
	if s.extract || isTarball(a.Name) {
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

	// Non-archive asset: drop it into dest under its own name, then mark it
	// executable when it looks runnable (a binary or a script), so a tool
	// fetched this way works without a manual chmod.
	if err := os.MkdirAll(s.dest, 0o755); err != nil {
		return err
	}
	out := filepath.Join(s.dest, destName(s, a.Name))
	if _, err := fetchToFile(client, a.URL, out, p, rp, "DL"); err != nil {
		return err
	}
	return makeExecutableIfRunnable(out)
}

// destName is the filename a single-file asset is saved under: the source's
// `as` override when set, otherwise the asset's own name.
func destName(s source, fallback string) string {
	if s.as != "" {
		return s.as
	}
	return fallback
}

// makeExecutableIfRunnable sets the execute bits on fp when its first bytes
// look like something meant to run: an ELF binary (covers AppImages too) or a
// "#!" script. Archives, .deb/.rpm, and plain data files are left untouched.
func makeExecutableIfRunnable(fp string) error {
	f, err := os.Open(fp)
	if err != nil {
		return err
	}
	var head [4]byte
	n, _ := io.ReadFull(f, head[:])
	f.Close()
	runnable := (n >= 4 && string(head[:]) == "\x7fELF") ||
		(n >= 2 && head[0] == '#' && head[1] == '!')
	if !runnable {
		return nil
	}
	fi, err := os.Stat(fp)
	if err != nil {
		return err
	}
	return os.Chmod(fp, fi.Mode()|0o111)
}

// fetchToFile streams url to out with a progress bar, following redirects (the
// CDN hop browser_download_url makes). It splits into a parallel/resumable
// download when prefs enable it and the (redirected) target advertises byte
// ranges; otherwise it falls back to a single stream.
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
	var n int64
	if p.splittable(resp) {
		resp.Body.Close() // drop the probe stream; range requests refetch
		n, err = saveSplit(client, resp, out, p, rp, prog)
	} else {
		n, err = saveStream(out, resp.Body, prog)
	}
	prog.finish()
	return n, err
}

// isTarball reports whether name is a tar archive gp can extract: a plain tar
// or one wrapped in any compression decompress() handles (gzip/zstd/xz/bzip2),
// in both the .tar.<ext> and short .t<ext> spellings.
func isTarball(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{
		".tar",
		".tar.gz", ".tgz",
		".tar.zst", ".tzst",
		".tar.xz", ".txz",
		".tar.bz2", ".tbz2", ".tbz",
	} {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
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
