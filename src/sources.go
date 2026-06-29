// Release resolution across git forges. The /releases/latest *web* page is
// HTML, but every forge exposes the same release as JSON over its REST API.
// Each forge differs in three things only — the endpoint URL, the JSON shape,
// and the auth header — so a small per-forge adapter normalizes all of them
// into the shared release/asset structs the rest of `up` consumes.
package src

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
// progress bar.
type asset struct {
	Name string
	URL  string
	Size int64
}

// release is the normalized slice of a forge's latest-release payload.
type release struct {
	TagName string
	Assets  []asset
}

// forge is one adapter: how to build the API URL for a repo, how to parse the
// response body, and how to authenticate. defaultHost is used when a source
// gives no host (GitHub is effectively single-instance; the others self-host).
type forge struct {
	name        string
	defaultHost string
	accept      string
	latestURL   func(host, repo string) string
	parse       func([]byte) (release, error)
	auth        func(*http.Request)
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

// fetchLatestRelease GETs the latest release of repo on the given forge/host,
// returning the normalized release. It sets Accept and a User-Agent (forges
// reject a missing UA) and applies the forge's token auth.
func fetchLatestRelease(client *http.Client, f forge, host, repo, ua string, rp retryPolicy) (release, error) {
	if host == "" {
		host = f.defaultHost
	}
	req, err := http.NewRequest(http.MethodGet, f.latestURL(forgeBase(host), repo), nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", f.accept)
	if ua == "" {
		ua = "gp" // forges reject a missing User-Agent
	}
	setUserAgent(req, ua)
	f.auth(req)

	resp, err := doRetry(client, req, rp)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if resp.StatusCode != http.StatusOK {
		// The body carries the forge's reason (rate limit, not found, ...).
		return release{}, fmt.Errorf("%s: %s: %s", repo, resp.Status, strings.TrimSpace(string(body)))
	}
	rel, err := f.parse(body)
	if err != nil {
		return release{}, fmt.Errorf("%s: %w", repo, err)
	}
	return rel, nil
}

// parseGitHubLike decodes the GitHub/Gitea/Forgejo release shape: a flat
// assets array with browser_download_url and size.
func parseGitHubLike(body []byte) (release, error) {
	var raw struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return release{}, fmt.Errorf("decode: %w", err)
	}
	rel := release{TagName: raw.TagName}
	for _, a := range raw.Assets {
		rel.Assets = append(rel.Assets, asset{Name: a.Name, URL: a.URL, Size: a.Size})
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
