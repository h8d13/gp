// Release resolution across git forges. The /releases/latest *web* page is
// HTML, but every forge exposes the same release as JSON over its REST API.
// Each forge differs in three things only — the endpoint URL, the JSON shape,
// and the auth header — so a small per-forge adapter normalizes all of them
// into the shared release/asset structs the rest of `up` consumes.
package base

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
)

// maxAPIBytes caps how much of a forge API or directory-index response gp reads
// into memory before parsing: ample for any real release JSON or listing, small
// enough to bound a hostile or runaway body.
const maxAPIBytes = 8 << 20

// asset is one downloadable file attached to a release, normalized across
// forges. URL may 302-redirect to a CDN, so the fetch path must follow
// redirects (the stdlib client does). Size is 0 when the forge omits it
// (GitLab release links carry no length), which just means an indeterminate
// progress bar. Digest is the asset's sha256 hex when the forge publishes one
// (GitHub does, as "sha256:<hex>"), so it can be verified with no config; "" if
// absent (GitLab links, directory indexes, older assets).
type asset struct {
	Name   string
	URL    string
	Size   int64
	Digest string
}

// release is the normalized slice of a forge's latest-release payload.
type release struct {
	TagName string
	Assets  []asset
}

// forge is one adapter: how to build the API URL for a repo, how to parse the
// response body, and how to authenticate. defaultHost is used when a source
// gives no host (GitHub is effectively single-instance; the others self-host).
// tagsURL/parseTags/archiveURL add `tag =` support (latest tag's source
// archive); they are nil where unsupported (GitHub, whose tags carry no order).
type forge struct {
	name        string
	defaultHost string
	accept      string
	latestURL   func(host, repo string) string
	parse       func([]byte) (release, error)
	auth        func(*http.Request)
	tagsURL     func(host, repo string) string      // list tags, newest first
	parseTags   func([]byte) ([]string, error)      // tag names from that body
	archiveURL  func(host, repo, tag string) string // source archive for a tag
}

// forges is the registry keyed by the `forge` ini value. Gitea/Forgejo/
// Codeberg share GitHub's release JSON, so one parser serves all of them;
// only the host, auth keyword, and API base differ. GitLab is distinct:
// projects are addressed by URL-encoded path and assets nest under
// assets.links with no size.
var forges = map[string]forge{
	"github": {
		name:        "github",
		defaultHost: "api.github.com",
		accept:      "application/vnd.github+json",
		latestURL: func(base, repo string) string {
			return fmt.Sprintf("%s/repos/%s/releases/latest", base, repo)
		},
		parse: parseGitHubLike,
		auth:  bearerAuth("GITHUB_TOKEN"),
		// archiveURL but no tagsURL: a pinned `tag = <version>` works, but
		// `tag = true` is refused because GitHub's /tags has no documented order
		// (it returns sub-crate tags first on a monorepo). Archives live on
		// github.com, not the api host; GitHub is single-instance, so hardcode it.
		archiveURL: func(base, repo, tag string) string {
			return fmt.Sprintf("https://github.com/%s/archive/refs/tags/%s.tar.gz", repo, tag)
		},
	},
	"gitea": {
		name:        "gitea",
		defaultHost: "codeberg.org", // most common public Gitea/Forgejo host
		accept:      "application/json",
		latestURL: func(base, repo string) string {
			return fmt.Sprintf("%s/api/v1/repos/%s/releases/latest", base, repo)
		},
		parse: parseGitHubLike,
		auth:  tokenAuth("GITEA_TOKEN"), // "Authorization: token <t>"
		tagsURL: func(base, repo string) string {
			return fmt.Sprintf("%s/api/v1/repos/%s/tags?limit=1", base, repo)
		},
		parseTags: parseTagNames,
		archiveURL: func(base, repo, tag string) string {
			return fmt.Sprintf("%s/%s/archive/%s.tar.gz", base, repo, tag)
		},
	},
	"gitlab": {
		name:        "gitlab",
		defaultHost: "gitlab.com",
		accept:      "application/json",
		latestURL: func(base, repo string) string {
			// GitLab addresses a project by its URL-encoded full path, so the
			// owner/repo slash becomes %2F.
			id := strings.ReplaceAll(url.PathEscape(repo), "/", "%2F")
			return fmt.Sprintf("%s/api/v4/projects/%s/releases/permalink/latest", base, id)
		},
		parse: parseGitLab,
		auth:  headerAuth("PRIVATE-TOKEN", "GITLAB_TOKEN"),
		tagsURL: func(base, repo string) string {
			id := strings.ReplaceAll(url.PathEscape(repo), "/", "%2F")
			return fmt.Sprintf("%s/api/v4/projects/%s/repository/tags?order_by=updated&per_page=1", base, id)
		},
		parseTags: parseTagNames,
		archiveURL: func(base, repo, tag string) string {
			// The web archive endpoint names the file <repo>-<tag>.tar.gz (a nice
			// extract dir), matching the URL GitLab's UI hands out.
			return fmt.Sprintf("%s/%s/-/archive/%s/%s-%s.tar.gz", base, repo, tag, path.Base(repo), tag)
		},
	},
}

// forgeBase turns a source's host into a scheme+authority. A bare host gets
// https:// (the norm); a host that already carries a scheme is used verbatim,
// which lets a self-hosted instance sit on http:// or a non-standard port.
func forgeBase(host string) string {
	if strings.Contains(host, "://") {
		return strings.TrimRight(host, "/")
	}
	return "https://" + host
}

// forgeFor resolves the adapter for a source's forge name, defaulting to
// github when unset.
func forgeFor(name string) (forge, error) {
	if name == "" {
		name = "github"
	}
	f, ok := forges[strings.ToLower(name)]
	if !ok {
		return forge{}, fmt.Errorf("unknown forge %q (want github, gitea, or gitlab)", name)
	}
	return f, nil
}

// bearerAuth/tokenAuth/headerAuth build the per-forge auth applier. Each reads
// its token from env and is a no-op when unset, so anonymous access still works
// (subject to the forge's lower unauthenticated rate limit).
func bearerAuth(env string) func(*http.Request) { return headerVal("Authorization", "Bearer ", env) }
func tokenAuth(env string) func(*http.Request)  { return headerVal("Authorization", "token ", env) }
func headerAuth(hdr, env string) func(*http.Request) {
	return func(r *http.Request) {
		if t := os.Getenv(env); t != "" {
			r.Header.Set(hdr, t)
		}
	}
}
func headerVal(hdr, prefix, env string) func(*http.Request) {
	return func(r *http.Request) {
		if t := os.Getenv(env); t != "" {
			r.Header.Set(hdr, prefix+t)
		}
	}
}

// forgeGET issues an authenticated forge API GET and returns the body. It sets
// Accept and a User-Agent (forges reject a missing UA) and applies the forge's
// token auth. A non-200 surfaces the body, which carries the forge's reason
// (rate limit, not found, ...).
func forgeGET(client *http.Client, f forge, reqURL, ua string, rp retryPolicy) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", f.accept)
	if ua == "" {
		ua = "gp"
	}
	setUserAgent(req, ua)
	f.auth(req)
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// fetchLatestRelease GETs the latest release of repo on the given forge/host,
// returning the normalized release.
func fetchLatestRelease(client *http.Client, f forge, host, repo, ua string, rp retryPolicy) (release, error) {
	if host == "" {
		host = f.defaultHost
	}
	body, err := forgeGET(client, f, f.latestURL(forgeBase(host), repo), ua, rp)
	if err != nil {
		return release{}, fmt.Errorf("%s: %w", repo, err)
	}
	rel, err := f.parse(body)
	if err != nil {
		return release{}, fmt.Errorf("%s: %w", repo, err)
	}
	return rel, nil
}

// parseTagNames decodes the GitLab/Gitea tag-list shape: an array of objects
// with a name. Both APIs return newest first, so element 0 is the latest tag.
func parseTagNames(body []byte) ([]string, error) {
	var raw []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode tags: %w", err)
	}
	names := make([]string, len(raw))
	for i, t := range raw {
		names[i] = t.Name
	}
	return names, nil
}

// fetchLatestTag returns the newest tag of repo on the forge/host, for a
// `tag = true` source.
func fetchLatestTag(client *http.Client, f forge, host, repo, ua string, rp retryPolicy) (string, error) {
	if host == "" {
		host = f.defaultHost
	}
	body, err := forgeGET(client, f, f.tagsURL(forgeBase(host), repo), ua, rp)
	if err != nil {
		return "", fmt.Errorf("%s: %w", repo, err)
	}
	tags, err := f.parseTags(body)
	if err != nil {
		return "", fmt.Errorf("%s: %w", repo, err)
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("%s: no tags", repo)
	}
	return tags[0], nil
}

// getJSON GETs reqURL and decodes the JSON body into v. It sends a User-Agent
// (Gitea/Forgejo reject a missing one) and a GITEA_TOKEN when set, mirroring the
// release path's auth. Used by the generic-package resolver.
func getJSON(client *http.Client, reqURL, ua string, rp retryPolicy, v any) error {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if ua == "" {
		ua = "gp"
	}
	setUserAgent(req, ua)
	if t := os.Getenv("GITEA_TOKEN"); t != "" {
		req.Header.Set("Authorization", "token "+t)
	}
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", reqURL, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, v)
}

// fetchLatestPackage resolves the newest version of a Forgejo/Gitea generic
// package (owner/name on host) into a normalized release: every file of that
// version becomes an asset whose URL is the registry download path, so pickAsset
// chooses among them by match/ext just as for a forge release. The package list
// is newest-first, so the first entry whose name matches exactly is the latest.
func fetchLatestPackage(client *http.Client, host, owner, name, ua string, rp retryPolicy) (release, error) {
	base := forgeBase(host)
	var pkgs []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	listURL := fmt.Sprintf("%s/api/v1/packages/%s?type=generic&q=%s",
		base, url.PathEscape(owner), url.QueryEscape(name))
	if err := getJSON(client, listURL, ua, rp, &pkgs); err != nil {
		return release{}, fmt.Errorf("%s: %w", name, err)
	}
	version := ""
	for _, pk := range pkgs {
		if pk.Name == name { // exact name (q= is a substring filter); newest-first
			version = pk.Version
			break
		}
	}
	if version == "" {
		return release{}, fmt.Errorf("%s: no generic package %q on %s", name, name, host)
	}

	var files []struct {
		Name string `json:"name"`
	}
	filesURL := fmt.Sprintf("%s/api/v1/packages/%s/generic/%s/%s/files",
		base, url.PathEscape(owner), url.PathEscape(name), url.PathEscape(version))
	if err := getJSON(client, filesURL, ua, rp, &files); err != nil {
		return release{}, fmt.Errorf("%s: %w", name, err)
	}
	rel := release{TagName: version}
	for _, f := range files {
		dl := fmt.Sprintf("%s/api/packages/%s/generic/%s/%s/%s",
			base, url.PathEscape(owner), url.PathEscape(name), url.PathEscape(version), url.PathEscape(f.Name))
		rel.Assets = append(rel.Assets, asset{Name: f.Name, URL: dl})
	}
	if len(rel.Assets) == 0 {
		return release{}, fmt.Errorf("%s: version %s has no files", name, version)
	}
	return rel, nil
}

// parseGitHubLike decodes the GitHub/Gitea/Forgejo release shape: a flat assets
// array with browser_download_url, size, and (GitHub) a digest. The digest is
// "<algo>:<hex>"; only sha256 is kept, since that is what verifyChecksum hashes
// with. An absent, null, or non-sha256 digest leaves Digest "" (no auto-verify).
func parseGitHubLike(body []byte) (release, error) {
	var raw struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return release{}, fmt.Errorf("decode: %w", err)
	}
	rel := release{TagName: raw.TagName}
	for _, a := range raw.Assets {
		digest, _ := strings.CutPrefix(a.Digest, "sha256:")
		if !isHex64(digest) {
			digest = "" // absent, null, or a non-sha256 algorithm
		}
		rel.Assets = append(rel.Assets, asset{Name: a.Name, URL: a.URL, Size: a.Size, Digest: digest})
	}
	return rel, nil
}

// parseGitLab decodes GitLab's shape: assets nest under assets.links, which
// carry no size and prefer direct_asset_url (the redirected, stable link).
func parseGitLab(body []byte) (release, error) {
	var raw struct {
		TagName string `json:"tag_name"`
		Assets  struct {
			Links []struct {
				Name           string `json:"name"`
				URL            string `json:"url"`
				DirectAssetURL string `json:"direct_asset_url"`
			} `json:"links"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return release{}, fmt.Errorf("decode: %w", err)
	}
	rel := release{TagName: raw.TagName}
	for _, l := range raw.Assets.Links {
		u := l.DirectAssetURL
		if u == "" {
			u = l.URL
		}
		rel.Assets = append(rel.Assets, asset{Name: l.Name, URL: u})
	}
	return rel, nil
}

// hrefRe pulls the target out of every href attribute on a page. A directory
// index (Apache/nginx/python -m http.server autoindex, and most distro
// mirrors) is just a list of <a href="file">; matching the attribute is looser
// than parsing HTML but needs no dependency, and the looseness is harmless:
// pickAsset's match/ext gate discards every link that is not the file the
// source asked for, so stray nav/sort/CSS hrefs never reach the caller.
var hrefRe = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)

// fetchIndex GETs a directory-index page and resolves its file links into the
// same normalized asset list a forge release produces, so pickAsset can choose
// among them by match/ext. base names the release for error messages.
func fetchIndex(client *http.Client, indexURL, ua string, rp retryPolicy) (release, error) {
	req, err := http.NewRequest(http.MethodGet, indexURL, nil)
	if err != nil {
		return release{}, err
	}
	if ua == "" {
		ua = "gp" // some index hosts reject a missing User-Agent
	}
	setUserAgent(req, ua)
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("%s: %s", indexURL, resp.Status)
	}
	return parseIndex(indexURL, body)
}

// parseIndex extracts file links from a directory-index body, each resolved
// against base into an absolute URL with the trailing path segment as its
// name. Links with no usable filename (the parent dir, "?C=N" sort headers,
// directory entries ending in "/") are skipped, and duplicates collapse.
func parseIndex(base string, body []byte) (release, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return release{}, fmt.Errorf("index url %q: %w", base, err)
	}
	rel := release{TagName: base} // base shows up in pickAsset's error context
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllSubmatch(body, -1) {
		ref, err := url.Parse(string(m[1]))
		if err != nil {
			continue
		}
		abs := bu.ResolveReference(ref)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue // mailto:, javascript:, ...
		}
		name := path.Base(abs.Path)
		if name == "" || name == "." || name == "/" || strings.HasSuffix(abs.Path, "/") {
			continue // a directory or parent link, not a file
		}
		abs.RawQuery, abs.Fragment = "", ""
		key := abs.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		rel.Assets = append(rel.Assets, asset{Name: name, URL: key})
	}
	if len(rel.Assets) == 0 {
		return release{}, fmt.Errorf("%s: no file links found", base)
	}
	return rel, nil
}

// pickAsset returns the single asset whose name contains every token in match
// (case-insensitive) and ends with ext. Zero or multiple matches are an error
// so the caller never silently grabs the wrong file; the message lists the
// candidates to help narrow the match/ext.
func (r release) pickAsset(match []string, ext string) (asset, error) {
	ext = strings.TrimPrefix(ext, ".")
	var hits []asset
	for _, a := range r.Assets {
		name := strings.ToLower(a.Name)
		if ext != "" && !strings.HasSuffix(name, "."+strings.ToLower(ext)) {
			continue
		}
		ok := true
		for _, m := range match {
			if m != "" && !strings.Contains(name, strings.ToLower(m)) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, a)
		}
	}

	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return asset{}, fmt.Errorf("no asset matches %v *.%s in %s; available: %s",
			match, ext, r.TagName, assetNames(r.Assets))
	default:
		return asset{}, fmt.Errorf("%d assets match %v *.%s (ambiguous); matched: %s",
			len(hits), match, ext, assetNames(hits))
	}
}

// assetNames joins asset names for an error message, capping the list so a
// release with dozens of assets does not flood the terminal.
func assetNames(as []asset) string {
	var names []string
	for i, a := range as {
		if i == 12 {
			names = append(names, fmt.Sprintf("... (+%d more)", len(as)-i))
			break
		}
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}
