package src

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// flagGroup records one option's aliases so usage prints it once as
// "-o, --output" instead of a line per name.
type flagGroup struct {
	names  []string
	typ    string
	usage  string
	global bool // true: applies to every command (incl. `up`); false: no-verb only
}

var flagGroups []flagGroup

// strVar/intVar bind every name to the same target: one definition, many
// spellings (-o and --output stay in sync). global tags which usage section
// the flag prints under (see usage()).
func strVar(p *string, global bool, def, usage string, names ...string) {
	for _, n := range names {
		flag.StringVar(p, n, def, usage)
	}
	flagGroups = append(flagGroups, flagGroup{names, "string", usage, global})
}

func intVar(p *int, global bool, def int, usage string, names ...string) {
	for _, n := range names {
		flag.IntVar(p, n, def, usage)
	}
	flagGroups = append(flagGroups, flagGroup{names, "int", usage, global})
}

func boolVar(p *bool, global bool, def bool, usage string, names ...string) {
	for _, n := range names {
		flag.BoolVar(p, n, def, usage)
	}
	flagGroups = append(flagGroups, flagGroup{names, "", usage, global})
}

// wasSet reports whether any of names was passed on the command line, so a
// flag can override config only when actually given (false != unset for bool).
func wasSet(names ...string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		for _, n := range names {
			if f.Name == n {
				set = true
			}
		}
	})
	return set
}

func usage() {
	out := flag.CommandLine.Output()
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(out, "Usage: %s [flags] URL\n", name)
	fmt.Fprintf(out, "       %s up [flags]\n", name)

	// Verbs first, then flags split by scope: global flags work for every
	// command (the URL form and `up`); download flags only for the URL form.
	fmt.Fprintf(out, "\nCommands:\n")
	fmt.Fprintf(out, "  %s up\n    \tinstall/update all tools from sources.ini\n", name)

	fmt.Fprintf(out, "\nGlobal options (all commands):\n")
	printFlags(out, true)

	fmt.Fprintf(out, "\nDownload options (URL form only):\n")
	printFlags(out, false)
}

// printFlags renders the flag groups whose scope matches global, one line per
// option with its aliases collapsed ("-o, --output").
func printFlags(out io.Writer, global bool) {
	for _, g := range flagGroups {
		if g.global != global {
			continue
		}
		var spell []string
		for _, n := range g.names {
			if len(n) == 1 {
				spell = append(spell, "-"+n)
			} else {
				spell = append(spell, "--"+n)
			}
		}
		line := strings.Join(spell, ", ")
		if g.typ != "" {
			line += " " + g.typ
		}
		fmt.Fprintf(out, "  %s\n    \t%s\n", line, g.usage)
	}
}

// tlsConfig is the single place TLS policy lives; nil means stdlib
// defaults. Both transports accept nil, so the insecure switch stays here.
func tlsConfig(p prefs) *tls.Config {
	if p.AllowInsecure {
		return &tls.Config{InsecureSkipVerify: true}
	}
	return nil
}

// setUserAgent sets req's User-Agent to ua, or leaves Go's default when ua is
// empty. gp's own requests pass the configured UA (possibly empty); forge API
// requests default ua to a non-empty value first, since forges reject a
// missing User-Agent.
func setUserAgent(req *http.Request, ua string) {
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
}

// newClient builds the HTTP client for the probe and the single-stream path.
// The QUIC path speaks HTTP/3 over a single QUIC connection (range requests
// become multiplexed streams, so they share one congestion window and do not
// aggregate past a per-connection cap); the default path is stdlib h1/h2,
// where h2 wins for a lone stream (one warmed window, cheaper than h1's extra
// per-request framing). A parallel split does NOT reuse this client: see
// newSplitClient for why h2 actively hurts there.
func newClient(p prefs) (*http.Client, func() error) {
	if p.Quic {
		tr := &http3.Transport{
			QUICConfig:      &quic.Config{HandshakeIdleTimeout: 15 * time.Second},
			TLSClientConfig: tlsConfig(p),
		}
		return &http.Client{Transport: tr}, tr.Close
	}

	// Bound only connect and header waits, never the body: a slow or
	// throttled large download must not be killed mid-transfer.
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true, // a custom TLSClientConfig otherwise disables h2
		TLSClientConfig:       tlsConfig(p),
		// Never let Go negotiate compression. Its transport only auto-handles
		// gzip, and doing so is wrong for a downloader: the Content-Length then
		// reports the compressed size (bad split math and progress), Apache's
		// mod_deflate drops Accept-Ranges (no parallel split), and it rewrites
		// the ETag ("abc" -> "abc-gzip") so a later conditional request never
		// matches and the 304 fast-path degrades to a full refetch. Identity is
		// also curl's default. --compress instead sets Accept-Encoding itself
		// (see acceptEncoding) and inflates the body via decodeBody, which
		// covers more codings than Go's built-in gzip and keeps this transport
		// out of it.
		DisableCompression: true,
	}
	return &http.Client{Transport: tr}, func() error { return nil }
}

// newSplitClient builds the client for a parallel range split. The point of a
// split is N congestion windows summing past a per-connection cap, but Go's h2
// transport multiplexes every range request as a stream over ONE TCP
// connection: N streams, one window, no aggregation (the same trap the QUIC
// path documents). So this client forces HTTP/1.1 (empty TLSNextProto disables
// h2) and sizes the connection pool to conns, giving each worker its own
// socket and its own window. MaxIdleConnsPerHost matters as much as the cap:
// the stdlib default is 2, which would tear down and re-dial all but two
// workers' connections between chunks. Falls back to newClient for QUIC, where
// the single-window tradeoff is the user's explicit choice.
func newSplitClient(p prefs) (*http.Client, func() error) {
	if p.Quic {
		return newClient(p)
	}
	conns := p.Parallel
	if conns < 1 {
		conns = 1
	}
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       tlsConfig(p),
		// non-nil empty map: opt out of the automatic h2 upgrade.
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:  true, // identity bytes; see newClient
		MaxConnsPerHost:     conns,
		MaxIdleConns:        conns,
		MaxIdleConnsPerHost: conns,
	}
	return &http.Client{Transport: tr}, func() error {
		tr.CloseIdleConnections()
		return nil
	}
}

// parseArgs parses flags that may appear before or after the URL. Go's
// flag package stops at the first non-flag token, so "gp URL -o file"
// would silently ignore -o; re-parse past each positional so flags and
// the URL work in any order. Returns the positional args in order.
func parseArgs() []string {
	var pos []string
	rest := os.Args[1:]
	for {
		flag.CommandLine.Parse(rest)
		if flag.NArg() == 0 {
			return pos
		}
		pos = append(pos, flag.Arg(0))
		rest = flag.Args()[1:]
	}
}

func urlScheme(url string, encrypt bool) string {
	if strings.Contains(url, "://") {
		return url
	}
	if encrypt {
		return "https://" + url
	}
	return "http://" + url
}

// Main is the CLI entry point, invoked by the root package's main().
func Main() {
	var out string
	strVar(&out, false, "", "save the response body to this path (otherwise stdout)", "o", "output")
	var par int
	intVar(&par, true, -1, "parallel connections; 1 disables", "p", "parallel")
	var useQuic bool
	boolVar(&useQuic, true, false, "use HTTP/3 over QUIC", "q", "quic")
	var compress bool
	boolVar(&compress, true, false, "accept gzip/zstd/br transfer encoding (disables ranges/resume/304)", "z", "compress")
	var ret int
	intVar(&ret, true, -1, "retries on 429/503; 0 disables", "r", "retries")
	var chk int
	intVar(&chk, true, -1, "split/resume chunk size in bytes", "c", "chunk")
	var noProg bool
	boolVar(&noProg, true, false, "disable the live download progress line", "n", "no-progress")
	var extract string
	strVar(&extract, false, "", "unpack the downloaded tar (gz/zst/xz/bz2) into this dir", "x", "extract")
	var force bool
	boolVar(&force, false, false, "re-download even if the cached copy is still current", "f", "force")
	flag.Usage = usage
	posArgs := parseArgs()

	p := loadPrefs(configPath(), ".env")
	if par >= 0 {
		p.Parallel = par
	}
	if wasSet("q", "quic") {
		p.Quic = useQuic
	}
	if wasSet("z", "compress") {
		p.Compress = compress
	}
	if ret >= 0 {
		p.Retries = ret
	}
	if chk > 0 {
		p.ChunkBytes = chk
	}
	// An explicit -c pins the span exactly (the user chose a resume
	// granularity); otherwise saveSplitAuto sizes it from the body size and
	// connection count once the probe reveals ContentLength.
	chunkSet := wasSet("c", "chunk")
	if noProg {
		p.Progress = false
	}

	// The `up` subcommand walks all of sources.ini using the prefs resolved
	// above, so the download flags (-p, -q, -r, -c, -n) shape its fetches too.
	// It takes no positional args; anything after `up` is ignored.
	if len(posArgs) > 0 && posArgs[0] == "up" {
		upMain(p)
		return
	}

	// No URL: a missing positional almost always means a flag swallowed it
	// (e.g. `gp -x URL`, where -x takes the extract dir as its value), so fail
	// loudly with usage rather than silently fetching some default.
	if len(posArgs) == 0 {
		fmt.Fprintln(os.Stderr, "gp: no URL given")
		flag.Usage()
		os.Exit(2)
	}
	url := posArgs[0]
	url = urlScheme(url, p.AlwaysEncrypt)
	if p.Quic && strings.HasPrefix(url, "http://") {
		url = "https://" + strings.TrimPrefix(url, "http://")
		fmt.Fprintln(os.Stderr, "quic: upgraded http:// to https:// (QUIC is TLS-only)")
	}

	client, closeClient := newClient(p)
	defer closeClient()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	setUserAgent(req, p.UserAgent)
	if p.Compress {
		// Opt into transfer compression for the codings decodeBody handles.
		// Forfeits ranges/resume/304 (the split case below also gates on this).
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}

	// Replay a prior run's validator as a conditional request, so an unchanged
	// file comes back 304 and skips the transfer (see applyCachedValidator for
	// the exact preconditions; --force opts out and always refetches).
	applyCachedValidator(req, out, url, force)

	rp := p.retry()
	start := time.Now()
	resp, err := doRetry(client, req, rp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	// The conditional matched: bytes on disk are current, nothing to fetch.
	// (With -x, re-extraction is skipped too; use --force if the dest is gone.)
	if resp.StatusCode == http.StatusNotModified {
		fmt.Fprintf(os.Stderr, "%s %s (cached: %s)\n", url, resp.Status, out)
		return
	}

	// Saving is explicit: only -o names an output file. With -x but no -o we
	// still need the bytes on disk, so stage a temp archive (removed after
	// extraction). With neither, the body is fetched and discarded -- the
	// status line still reports protocol and size.
	explicitOut := out != ""
	if out == "" && extract != "" {
		// Stage under os.TempDir(), which honors $TMPDIR ($TMPDIR else
		// /tmp on Unix) so the caller can place the transient archive on
		// a chosen filesystem (e.g. away from a small tmpfs).
		tf, err := os.CreateTemp("", "gp-archive-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "extract:", err)
			os.Exit(1)
		}
		out = tf.Name()
		tf.Close()
		defer os.Remove(out) // user asked for files, not the tarball
	}
	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			os.Exit(1)
		}
	}

	var n int64
	// A compressed body has no useful length up front (Content-Length is the
	// compressed size, the output is larger), so the bar runs indeterminate.
	total := resp.ContentLength
	if !isIdentity(resp.Header.Get("Content-Encoding")) {
		total = 0
	}
	// One progress bar for the whole run: it shows [DL] while downloading,
	// then reset()s to [XT] for extraction, reusing the same line.
	prog := newProgress(total, "DL", p.Progress)
	prog.run()
	switch {
	// Split only when it can pay off and offset-writes are possible; that path
	// also owns the output file and handles resume. Never split a compressed
	// transfer: byte ranges would address the encoded stream, not the file, so
	// the reassembled offsets would be meaningless (--compress forces one
	// stream, and a server compressing on its own drops Accept-Ranges anyway).
	case !p.Compress && out != "" && p.splittable(resp):
		resp.Body.Close() // drop the probe stream; range requests refetch
		n, err = saveSplitAuto(resp, out, p, chunkSet, rp, prog)
	default:
		// Single stream. Inflate any transfer compression on the fly; an
		// identity body passes through, so the common path is unchanged. With
		// -o the bytes go to the file; without it they stream to stdout so a
		// bare fetch stays pipeable (gp URL | tar xz), diagnostics on stderr.
		body, closeBody, derr := decodeBody(resp)
		if derr != nil {
			err = derr
		} else {
			if closeBody != nil {
				defer closeBody()
			}
			if out != "" {
				n, err = saveStream(out, body, prog)
			} else {
				n, err = io.Copy(progWriter{os.Stdout, prog}, body)
			}
		}
	}
	if err != nil {
		prog.finish()
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	elapsed := time.Since(start) // download time, reported below

	// Record the validator so the next run for this URL can ask "still
	// unchanged?" and skip the transfer. Best-effort: a write failure only
	// forfeits the 304 fast-path next time, it never fails the download.
	if explicitOut && resp.StatusCode == http.StatusOK {
		if m, ok := metaFrom(url, resp); ok {
			_ = m.save(metaPath(out))
		}
	}

	var cnt int
	if extract != "" {
		if fi, statErr := os.Stat(out); statErr == nil {
			prog.reset(fi.Size(), "XT")
		}
		cnt, err = extractTarGz(out, extract, prog)
		if err != nil {
			prog.finish()
			fmt.Fprintln(os.Stderr, "extract:", err)
			os.Exit(1)
		}
	}
	prog.finish()

	dest := ""
	if explicitOut {
		dest = " saved=" + out
	}
	// Summary on stderr: stdout is reserved for the body (no-o case).
	fmt.Fprintf(os.Stderr, "%s %s proto=%s bytes=%d in %v%s\n", url, resp.Status, resp.Proto, n, elapsed, dest)
	if extract != "" {
		fmt.Fprintf(os.Stderr, "extracted %d entries to %s\n", cnt, extract)
	}
}
