package base

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

// flagGroup keeps one option's aliases so usage prints "-o, --output" once.
type flagGroup struct {
	names []string
	typ   string
	usage string
}

var flagGroups []flagGroup

// Every alias binds the same target, so -o and --output stay in sync.
func strVar(p *string, def, usage string, names ...string) {
	for _, n := range names {
		flag.StringVar(p, n, def, usage)
	}
	flagGroups = append(flagGroups, flagGroup{names, "string", usage})
}

func intVar(p *int, def int, usage string, names ...string) {
	for _, n := range names {
		flag.IntVar(p, n, def, usage)
	}
	flagGroups = append(flagGroups, flagGroup{names, "int", usage})
}

func boolVar(p *bool, def bool, usage string, names ...string) {
	for _, n := range names {
		flag.BoolVar(p, n, def, usage)
	}
	flagGroups = append(flagGroups, flagGroup{names, "", usage})
}

// wasSet lets a flag override config only when given (false != unset).
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
	fmt.Fprintf(out, "\nOptions:\n")
	printFlags(out)
}

func printFlags(out io.Writer) {
	for _, g := range flagGroups {
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

// One shared session cache: split connections 2..N resume the first TLS
// session (one fewer round trip). A custom config disables h2 unless
// ForceAttemptHTTP2 is set (see newClient).
func tlsConfig(p prefs) *tls.Config {
	cfg := &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(0)}
	if p.AllowInsecure {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

// Empty ua keeps Go's default.
func setUserAgent(req *http.Request, ua string) {
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
}

// newClient serves the probe and the single-stream path, where h2 wins.
// Splits use newSplitClient: h2 and QUIC multiplex ranges onto one
// congestion window.
func newClient(p prefs) (*http.Client, func() error) {
	if p.Quic {
		tr := &http3.Transport{
			QUICConfig: &quic.Config{
				HandshakeIdleTimeout: 15 * time.Second,
			},
			TLSClientConfig: tlsConfig(p),
		}
		return &http.Client{Transport: tr}, tr.Close
	}

	tr := baseTransport(p)
	tr.ForceAttemptHTTP2 = true // a custom TLSClientConfig otherwise disables h2
	return &http.Client{Transport: tr}, func() error { return nil }
}

// baseTransport bounds connect and header waits only: a throttled body must
// not be killed mid-transfer.
func baseTransport(p prefs) *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       tlsConfig(p),
		// Go's auto-gzip breaks a downloader: Content-Length becomes the
		// compressed size (bad split math, progress), mod_deflate drops
		// Accept-Ranges, and the ETag is rewritten ("abc" -> "abc-gzip") so
		// 304s never match. --compress sets Accept-Encoding and decodes itself.
		DisableCompression: true,
	}
}

// newSplitClient forces HTTP/1.1 with one socket per worker: Go's h2 puts
// every range on one TCP connection, one window, no aggregation. The
// MaxIdleConnsPerHost default of 2 would re-dial the rest between chunks.
// QUIC keeps its single connection: the user chose it.
func newSplitClient(p prefs) (*http.Client, func() error) {
	if p.Quic {
		return newClient(p)
	}
	conns := max(p.Parallel, 1)
	tr := baseTransport(p)
	// non-nil empty map: opt out of the automatic h2 upgrade
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.MaxConnsPerHost = conns
	tr.MaxIdleConns = conns
	tr.MaxIdleConnsPerHost = conns
	return &http.Client{Transport: tr}, func() error {
		tr.CloseIdleConnections()
		return nil
	}
}

// flag stops at the first positional, so "gp URL -o f" would drop -o;
// re-parse after each positional.
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

func Main() {
	var out string
	strVar(&out, "", "save the response body to this path (otherwise stdout)",
		"o", "output")
	var par int
	intVar(&par, -1, "parallel connections; 1 disables", "p", "parallel")
	var useQuic bool
	boolVar(&useQuic, false, "use HTTP/3 over QUIC", "q", "quic")
	var compress bool
	boolVar(&compress, false,
		"accept gzip/zstd/br transfer encoding (disables ranges/resume/304)",
		"z", "compress")
	var ret int
	intVar(&ret, -1, "retries on 429/503; 0 disables", "r", "retries")
	var chk int
	intVar(&chk, -1, "split/resume chunk size in bytes", "c", "chunk")
	var noProg bool
	boolVar(&noProg, false, "disable the live download progress line",
		"n", "no-progress")
	var extract string
	strVar(&extract, "", "unpack the downloaded tar (gz/zst/xz/bz2) into this dir",
		"x", "extract")
	var force bool
	boolVar(&force, false, "re-download even if the cached copy is still current",
		"f", "force")
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
	// -c pins the span; otherwise saveSplitAuto sizes it from ContentLength.
	chunkSet := wasSet("c", "chunk")
	if noProg {
		p.Progress = false
	}

	// Usually a flag swallowed the URL (`gp -x URL`): fail loudly.
	if len(posArgs) == 0 {
		fmt.Fprintln(os.Stderr, "gp: no URL given")
		flag.Usage()
		os.Exit(2)
	}
	url := posArgs[0]
	url = urlScheme(url, p.AlwaysEncrypt)
	if p.Quic && strings.HasPrefix(url, "http://") {
		url = "https://" + strings.TrimPrefix(url, "http://")
		fmt.Fprintln(os.Stderr,
			"quic: upgraded http:// to https:// (QUIC is TLS-only)")
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
		// Forfeits ranges/resume/304.
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}

	applyCachedValidator(req, out, url, force)

	rp := p.retry()
	start := time.Now()
	resp, err := doRetry(client, req, rp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	// Unchanged on disk. -x is skipped too; --force if dest was removed.
	if resp.StatusCode == http.StatusNotModified {
		fmt.Fprintf(os.Stderr, "%s %s (cached: %s)\n", url, resp.Status, out)
		return
	}

	// Only -o saves; -x alone stages a temp archive; with neither, stdout.
	explicitOut := out != ""
	if out == "" && extract != "" {
		// honors $TMPDIR, e.g. to keep big archives off a small tmpfs
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
	// Compressed Content-Length isn't the output size: indeterminate bar.
	total := resp.ContentLength
	if !isIdentity(resp.Header.Get("Content-Encoding")) {
		total = 0
	}
	// One bar for the run: [DL], then reset to [XT] for extraction.
	prog := newProgress(total, "DL", p.Progress)
	prog.run()
	switch {
	// Never split a compressed transfer: ranges address the encoded stream.
	case !p.Compress && out != "" && p.splittable(resp):
		resp.Body.Close() // drop the probe stream; range requests refetch
		n, err = saveSplitAuto(resp, out, p, chunkSet, rp, prog)
	default:
		// No -o: stream to stdout so `gp URL | tar xz` works.
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
	elapsed := time.Since(start)

	// Best-effort: a failed write only costs the next run's 304.
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
	// summary on stderr: stdout carries the body
	fmt.Fprintf(os.Stderr, "%s %s proto=%s bytes=%d in %v%s\n",
		url, resp.Status, resp.Proto, n, elapsed, dest)
	if extract != "" {
		fmt.Fprintf(os.Stderr, "extracted %d entries to %s\n", cnt, extract)
	}
}
