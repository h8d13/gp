// Optional sha256 verification for `up`. A source may pin a digest (sha256) or
// point at a published checksums file (sha256-url); either way the downloaded
// asset is hashed and compared before it is placed or extracted, so a tampered
// or truncated release is never installed. A source that pins nothing skips all
// of this -- verification is opt-in per source.
package base

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// verifyChecksum hashes the file at fp and checks it against the expected
// sha256, resolved in precedence order: the source's pinned digest, else the
// one looked up by asset name in its sha256-url checksums file, else the digest
// the forge published for the asset (GitHub ships one, so its releases verify
// with no config). It is a no-op when none of those exist. A mismatch is a hard
// error.
func verifyChecksum(client *http.Client, s source, a asset, fp string, p prefs, rp retryPolicy) error {
	want := s.sha256
	if want == "" && s.sumURL != "" {
		got, err := fetchChecksum(client, s.sumURL, a.Name, p.UserAgent, rp)
		if err != nil {
			return err
		}
		want = got
	}
	if want == "" {
		want = a.Digest // forge-published per-asset digest, if any
	}
	if want == "" {
		return nil
	}
	sum, err := sha256File(fp)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, want) {
		return fmt.Errorf("checksum mismatch for %s: want %s, got %s", a.Name, want, sum)
	}
	return nil
}

// sha256File returns the lowercase hex sha256 of the file at fp, streaming it so
// an arbitrarily large asset never lands in memory.
func sha256File(fp string) (string, error) {
	f, err := os.Open(fp)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchChecksum GETs a checksums file and returns the sha256 recorded for the
// asset named name. See parseChecksum for the accepted formats.
func fetchChecksum(client *http.Client, sumURL, name, ua string, rp retryPolicy) (string, error) {
	req, err := http.NewRequest(http.MethodGet, sumURL, nil)
	if err != nil {
		return "", err
	}
	setUserAgent(req, ua)
	resp, err := doRetry(client, req, rp)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum %s: %s", sumURL, resp.Status)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	return parseChecksum(body, name)
}

// parseChecksum finds the sha256 for name in a checksums file. It accepts the
// two shapes seen in the wild: a `sha256sum`-style listing, one entry per line
// as `<hex> [*]<filename>` (the leading `*` marks binary mode), matched on the
// filename's basename; and a single bare `<hex>` (a per-asset `.sha256` file),
// which applies to whatever asset it was fetched for.
func parseChecksum(body []byte, name string) (string, error) {
	name = path.Base(name)
	bare := ""
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
			continue
		case len(f) == 1:
			if isHex64(f[0]) {
				bare = f[0] // a lone digest: a per-asset .sha256
			}
		default:
			fn := strings.TrimPrefix(f[len(f)-1], "*")
			if isHex64(f[0]) && path.Base(fn) == name {
				return strings.ToLower(f[0]), nil
			}
		}
	}
	if bare != "" {
		return strings.ToLower(bare), nil
	}
	return "", fmt.Errorf("no sha256 for %s in checksums file", name)
}

// isHex64 reports whether s is exactly 64 hex digits: the textual shape of a
// sha256 digest. Used to validate a pinned digest and to spot digest tokens in
// a checksums file.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
