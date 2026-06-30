// The `up` subcommand: keep locally-installed tools current. A sources.ini next
// to config.ini lists [NAME] sections; each resolves to a downloadable file one
// of several ways (a forge release, bare url, directory index, generic package,
// or git-tag archive; see the source struct), and `gp up` installs it only when
// the upstream version changed. The version is the release tag / package version
// / git tag for an API source, else the HTTP validator (ETag/Last-Modified). A
// lockfile records what is installed, so an unchanged upstream is a no-op.
package base

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

// source is one [NAME] section of sources.ini. Its type is set by exactly one
// of url / index / package / repo (resolved in that precedence; see
// loadSources), and the other type fields are then ignored.
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
	sha256  string   // pinned sha256 hex; the asset must hash to this
	sumURL  string   // URL of a checksums file to look the asset's sha256 up in
	pkg     string   // owner/name of a Forgejo/Gitea generic package (host sets the instance)
	tag     string   // with repo: install a git tag's source archive (true=latest, else pinned)
}

// sourcesPath and lockPath live beside config.ini so all gp state is in one
// place under $XDG_CONFIG_HOME/gp.
func sourcesPath() string { return filepath.Join(filepath.Dir(configPath()), "sources.ini") }
func lockPath() string    { return filepath.Join(filepath.Dir(configPath()), "sources.lock") }

// loadSources parses sources.ini into sources in file order (top to bottom), so
// `gp up` installs them in the order you wrote and you can sequence by priority
// or dependency. Each section needs a source type and dest; ext and match are
// optional (no ext means match by substring only).
func loadSources(path string) ([]source, error) {
	cfg, order := loadConfig(path)
	if len(cfg) == 0 {
		return nil, fmt.Errorf("no sources in %s", path)
	}

	var srcs []source
	for _, name := range order {
		sec := cfg[name]
		s := source{
			name:   name,
			url:    sec["url"],
			index:  sec["index"],
			forge:  sec["forge"],
			host:   sec["host"],
			repo:   sec["repo"],
			ext:    sec["ext"],
			as:     sec["as"],
			dest:   expandHome(sec["dest"]),
			sha256: strings.ToLower(sec["sha256"]),
			sumURL: sec["sha256-url"],
			pkg:    sec["package"],
			tag:    sec["tag"],
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
		if s.sha256 != "" && s.sumURL != "" {
			return nil, fmt.Errorf("[%s]: set sha256 or sha256-url, not both", name)
		}
		if s.sha256 != "" && !isHex64(s.sha256) {
			return nil, fmt.Errorf("[%s]: sha256 must be 64 hex chars", name)
		}
		if s.tag != "" && s.repo == "" {
			return nil, fmt.Errorf("[%s]: tag needs repo (it installs that repo's tag archive)", name)
		}
		// Resolve the source type, in precedence order: a bare url is the file
		// itself; an index is a listing to pick one file from; a package is a
		// Forgejo/Gitea generic-registry entry; otherwise it is a forge release.
		// Each later field is ignored once an earlier one wins.
		switch {
		case s.url != "":
		case s.index != "":
			if len(s.match) == 0 && s.ext == "" {
				return nil, fmt.Errorf("[%s]: index needs match or ext to pick a file", name)
			}
		case s.pkg != "":
			if _, _, ok := splitOwnerName(s.pkg); !ok {
				return nil, fmt.Errorf("[%s]: package must be owner/name", name)
			}
			if s.host == "" {
				return nil, fmt.Errorf("[%s]: package needs host (the Forgejo/Gitea instance, e.g. codeberg.org)", name)
			}
			if len(s.match) == 0 && s.ext == "" {
				return nil, fmt.Errorf("[%s]: package needs match or ext to pick a file", name)
			}
		case s.repo != "":
			f, err := forgeFor(s.forge)
			if err != nil {
				return nil, fmt.Errorf("[%s]: %w", name, err)
			}
			if s.tag != "" && f.archiveURL == nil {
				return nil, fmt.Errorf("[%s]: forge %q has no tag-archive support", name, f.name)
			}
			if tagIsLatest(s.tag) && f.tagsURL == nil {
				return nil, fmt.Errorf("[%s]: forge %q has no ordered tag list, so tag = true can't find the newest; use a literal tag (tag = <name>, e.g. tag = latest) or a release source", name, f.name)
			}
		default:
			return nil, fmt.Errorf("[%s]: url, index, package, or repo is required", name)
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

// contractHome is expandHome's inverse for display: a path under the home dir
// is shown with a leading ~ (as the user wrote it in sources.ini), anything
// else unchanged.
func contractHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

// upMain is the `gp up` entry point: it installs/updates every source in
// sources.ini. With checkOnly it parses the config, then probes each source over
// the network (resolve the latest version, confirm match/ext pick one asset)
// without downloading: the `gp up check` lint. It exits non-zero if any source
// fails to parse or resolve.
func upMain(p prefs, checkOnly bool) {
	srcs, err := loadSources(sourcesPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "up:", err)
		os.Exit(1)
	}
	if checkOnly {
		client, closeClient := newClient(p)
		defer closeClient()
		rp := p.retry()
		failed := false
		for _, s := range srcs {
			desc, err := probeOne(client, s, p, rp)
			if err != nil {
				fmt.Printf("%s: unresolved (%v)\n", s.name, err)
				failed = true
				continue
			}
			fmt.Printf("%s: ok, %s\n", s.name, desc)
		}
		if failed {
			os.Exit(1)
		}
		return
	}

	lock, _ := loadConfig(lockPath()) // [installed] NAME = tag
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

// rmMain implements `gp rm <name>...`: it forgets each named source (drops the
// source's lock entry so a later `up` reinstalls it fresh) and prints where its
// files live. It deletes nothing on purpose, since a dest can be shared with
// other sources or unrelated files (e.g. ~/.local/bin); removal is left to the
// user. Names must exist in sources.ini, which is where dest is read from.
func rmMain(names []string) {
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "rm: need at least one source name")
		os.Exit(2)
	}
	srcs, err := loadSources(sourcesPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "rm:", err)
		os.Exit(1)
	}
	byName := make(map[string]source, len(srcs))
	for _, s := range srcs {
		byName[s.name] = s
	}
	lock, _ := loadConfig(lockPath())
	installed := lock["installed"]
	if installed == nil {
		installed = map[string]string{}
	}

	failed, changed := false, false
	for _, name := range names {
		s, ok := byName[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "rm: no source named %q in %s\n", name, sourcesPath())
			failed = true
			continue
		}
		if _, tracked := installed[name]; tracked {
			delete(installed, name)
			changed = true
		}
		fmt.Printf("%s: dropped from lock; delete its files yourself: %s\n", name, contractHome(s.dest))
	}
	if changed {
		if err := saveLock(lockPath(), installed); err != nil {
			fmt.Fprintln(os.Stderr, "rm: write lock:", err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// syncOne resolves s to its latest version and installs it when the tag differs
// from have (the recorded installed tag) or the destination is missing. It
// returns the newly installed tag, or "" when nothing changed. url/index/package
// sources have their own resolvers; the fallthrough is a forge release.
func syncOne(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	if s.url != "" {
		return syncURL(client, s, have, p, rp)
	}
	if s.index != "" {
		return syncIndex(client, s, have, p, rp)
	}
	if s.pkg != "" {
		return syncPackage(client, s, have, p, rp)
	}
	if s.tag != "" {
		return syncTag(client, s, have, p, rp)
	}
	f, err := forgeFor(s.forge) // already validated in loadSources
	if err != nil {
		return "", err
	}
	rel, err := fetchLatestRelease(client, f, s.host, s.repo, p.UserAgent, rp)
	if err != nil {
		return "", err
	}
	return installRelease(client, s, rel, have, p, rp)
}

// syncTag handles a `repo` source with `tag` set: it installs the source archive
// of a git tag (a tarball, auto-extracted). tag = true (or latest) resolves the
// newest tag via the forge's tags API, so it auto-updates; any other value pins
// that exact tag. The tag is the lock version. Only forges with archive support
// (gitlab, gitea/forgejo) reach here; loadSources rejects the rest.
func syncTag(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	f, err := forgeFor(s.forge)
	if err != nil {
		return "", err
	}
	host := s.host
	if host == "" {
		host = f.defaultHost
	}
	base := forgeBase(host)
	if tagIsLatest(s.tag) {
		// tag = true: resolve the newest tag. Its name is the version, so a new
		// tag reads as an update (v1 -> v2).
		tag, err := fetchLatestTag(client, f, host, s.repo, p.UserAgent, rp)
		if err != nil {
			return "", err
		}
		dl := f.archiveURL(base, s.repo, tag)
		rel := release{TagName: tag, Assets: []asset{{Name: urlBase(dl), URL: dl}}}
		return installRelease(client, s, rel, have, p, rp)
	}
	// A literal tag: a fixed version, or a rolling tag (latest/nightly) that is
	// force-moved. Version by the archive's HTTP validator like a bare url, so a
	// fixed tag is a no-op and a moved rolling tag is refetched when its bytes
	// change -- the tag name alone could not tell those apart.
	dl := f.archiveURL(base, s.repo, s.tag)
	return installFile(client, s, destName(s, urlBase(dl)), dl, have, p, rp)
}

// tagIsLatest reports whether `tag` asks gp to resolve the newest tag via the
// API. Only the literal true does; everything else (including "latest") is a
// real tag name, since rolling "latest"/"nightly" tags are common.
func tagIsLatest(v string) bool { return v == "true" }

// syncPackage handles a generic-package source: resolve the newest version on
// the Forgejo/Gitea instance named by host (required), then install like a
// release. forge plays no part: the generic-registry API is identical on every
// instance, so host alone identifies it.
func syncPackage(client *http.Client, s source, have string, p prefs, rp retryPolicy) (string, error) {
	owner, name, _ := splitOwnerName(s.pkg) // validated in loadSources
	rel, err := fetchLatestPackage(client, s.host, owner, name, p.UserAgent, rp)
	if err != nil {
		return "", err
	}
	return installRelease(client, s, rel, have, p, rp)
}

// installRelease is the shared tail for release-shaped sources (forge release
// and generic package): skip when the tag is unchanged and dest is intact, else
// pick the asset, announce, and install. Returns the tag to record, or "".
func installRelease(client *http.Client, s source, rel release, have string, p prefs, rp retryPolicy) (string, error) {
	if rel.TagName == have && destPopulated(s.dest) {
		fmt.Printf("%s: up to date (%s)\n", s.name, rel.TagName)
		return "", nil
	}
	asset, err := rel.pickAsset(s.match, s.ext)
	if err != nil {
		return "", err
	}
	switch {
	case have == "":
		fmt.Printf("%s: installing %s (%s)\n", s.name, rel.TagName, asset.Name)
	case have == rel.TagName:
		// Same tag, but the up-to-date check above fell through, so dest was
		// missing: this is a reinstall, not an update.
		fmt.Printf("%s: reinstalling %s (%s)\n", s.name, rel.TagName, asset.Name)
	default:
		fmt.Printf("%s: %s -> %s (%s)\n", s.name, have, rel.TagName, asset.Name)
	}

	if err := install(client, asset, s, p, rp); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// splitOwnerName splits an "owner/name" pair; ok is false unless both halves are
// non-empty and there is exactly one slash.
func splitOwnerName(s string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(s, "/")
	return owner, name, ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

// headOK reports nil when reqURL answers 200 to a HEAD: a reachability probe
// (used by `up check`) that downloads nothing.
func headOK(client *http.Client, reqURL, ua string, rp retryPolicy) error {
	req, err := http.NewRequest(http.MethodHead, reqURL, nil)
	if err != nil {
		return err
	}
	setUserAgent(req, ua)
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", reqURL, resp.Status)
	}
	return nil
}

// probeOne resolves a source over the network without downloading: it confirms
// the latest version/listing is reachable and that match/ext pick exactly one
// asset, returning a one-line description. This is the work behind `up check`,
// so a wrong match, a moved repo, or an unreachable host is caught before a real
// `up` (or a CI run) rather than mid-install.
func probeOne(client *http.Client, s source, p prefs, rp retryPolicy) (string, error) {
	pick := func(rel release) (string, error) {
		a, err := rel.pickAsset(s.match, s.ext)
		if err != nil {
			return "", err
		}
		if rel.TagName != "" {
			return fmt.Sprintf("%s (%s)", a.Name, rel.TagName), nil
		}
		return a.Name, nil
	}
	switch {
	case s.url != "":
		if err := headOK(client, s.url, p.UserAgent, rp); err != nil {
			return "", err
		}
		return urlBase(s.url), nil
	case s.index != "":
		rel, err := fetchIndex(client, s.index, p.UserAgent, rp)
		if err != nil {
			return "", err
		}
		return pick(rel)
	case s.pkg != "":
		owner, name, _ := splitOwnerName(s.pkg)
		rel, err := fetchLatestPackage(client, s.host, owner, name, p.UserAgent, rp)
		if err != nil {
			return "", err
		}
		return pick(rel)
	case s.tag != "":
		f, err := forgeFor(s.forge)
		if err != nil {
			return "", err
		}
		tag := s.tag
		if tagIsLatest(tag) {
			if tag, err = fetchLatestTag(client, f, s.host, s.repo, p.UserAgent, rp); err != nil {
				return "", err
			}
		}
		host := s.host
		if host == "" {
			host = f.defaultHost
		}
		dl := f.archiveURL(forgeBase(host), s.repo, tag)
		if err := headOK(client, dl, p.UserAgent, rp); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s (%s)", urlBase(dl), tag), nil
	default:
		f, err := forgeFor(s.forge)
		if err != nil {
			return "", err
		}
		rel, err := fetchLatestRelease(client, f, s.host, s.repo, p.UserAgent, rp)
		if err != nil {
			return "", err
		}
		return pick(rel)
	}
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
	switch {
	case have == "":
		fmt.Printf("%s: installing %s\n", s.name, name)
	case validator != "" && validator == have:
		// Validator unchanged but dest was missing: a reinstall, not an update.
		fmt.Printf("%s: reinstalling %s\n", s.name, name)
	default:
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
	setUserAgent(req, ua)
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
// no usable name, e.g. GitHub's /tarball API). An archive is staged in a temp
// file and removed afterwards. One progress bar spans the whole install,
// relabeling [DL] -> [XT] in place; verification happens in between but is too
// fast for its own phase, so it leaves a persistent [VF✓] marker on the bar.
func install(client *http.Client, a asset, s source, p prefs, rp retryPolicy) error {
	// Stage an archive in a temp file (verified before it can reach s.dest); a
	// plain file downloads straight to its final path under s.dest.
	archive := s.extract || isTarball(a.Name)
	var fp string
	if archive {
		tmp, err := os.CreateTemp("", "gp-up-*")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		fp = tmp.Name()
	} else {
		if err := os.MkdirAll(s.dest, 0o755); err != nil {
			return err
		}
		fp = filepath.Join(s.dest, destName(s, a.Name))
	}

	prog := newProgress(0, "DL", p.Progress)
	prog.run()

	if _, err := fetchToFile(client, a.URL, fp, p, rp, prog); err != nil {
		prog.finish()
		if !archive {
			os.Remove(fp) // no half-written file left in dest
		}
		return err
	}
	src, err := verifyChecksum(client, s, a, fp, p, rp)
	if err != nil {
		prog.finish()
		os.Remove(fp) // never leave an unverified file staged or in dest
		return err
	}
	if src != "" {
		prog.setNote("[VF✓]") // a lasting verify mark on the bar
	}

	if archive {
		var size int64
		if fi, e := os.Stat(fp); e == nil {
			size = fi.Size()
		}
		prog.reset(size, "XT")
		if _, err := extractTarGz(fp, s.dest, prog); err != nil {
			prog.finish()
			return err
		}
	}
	prog.finish()
	if !archive {
		// Mark a binary or "#!" script executable so it runs without a chmod.
		return makeExecutableIfRunnable(fp)
	}
	return nil
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

// fetchToFile streams url to out, following redirects (the CDN hop
// browser_download_url makes). It splits into a parallel/resumable download when
// prefs enable it and the (redirected) target advertises byte ranges; otherwise
// it falls back to a single stream. It reports into the caller's prog (relabeled
// to [DL]); the caller owns the bar's run/finish so it can span later phases.
func fetchToFile(client *http.Client, url, out string, p prefs, rp retryPolicy, prog *progress) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	setUserAgent(req, p.UserAgent)
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
	prog.reset(resp.ContentLength, "DL")
	var n int64
	if p.splittable(resp) {
		resp.Body.Close() // drop the probe stream; range requests refetch
		// up never pins the segment, so it always auto-sizes; a configured
		// chunk still floors the span inside splitSegment.
		n, err = saveSplitAuto(resp, out, p, false, rp, prog)
	} else {
		n, err = saveStream(out, resp.Body, prog)
	}
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
