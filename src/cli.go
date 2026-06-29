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
	names []string
	typ   string
	usage string
}

var flagGroups []flagGroup

// strVar/intVar bind every name to the same target: one definition, many
// spellings (-o and --output stay in sync).
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
	// Subcommands get their own block so they don't read as flags of the
	// default URL form above.
	fmt.Fprintf(out, "\nCommands:\n")
	fmt.Fprintf(out, "  %s up [NAME...]\n    \tinstall/update tools from sources.ini\n", name)
}

// tlsConfig is the single place TLS policy lives; nil means stdlib
// defaults. Both transports accept nil, so the insecure switch stays here.
func tlsConfig(p prefs) *tls.Config {
	if p.AllowInsecure {
		return &tls.Config{InsecureSkipVerify: true}
	}
	return nil
}

// newClient builds the HTTP client for p. The QUIC path speaks HTTP/3 over a
// single QUIC connection (range requests become multiplexed streams, so they
// share one congestion window and do not aggregate past a per-connection
// cap); the default path is stdlib h1/h2. The returned closer releases the
// QUIC transport and is a no-op otherwise.
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
	}
	return &http.Client{Transport: tr}, func() error { return nil }
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
	// The `up` subcommand is a separate verb from the URL fetcher: handle it
	// before flag parsing so its args don't collide with the download flags.
	if len(os.Args) > 1 && os.Args[1] == "up" {
		upMain(os.Args[2:], loadPrefs(configPath(), ".env"))
		return
	}

	var out string
	strVar(&out, "", "save the response body to this path (otherwise stdout)", "o", "output")
	var par int
	intVar(&par, -1, "parallel connections; 1 disables", "p", "parallel")
	var useQuic bool
	boolVar(&useQuic, false, "use HTTP/3 over QUIC", "q", "quic")
	var ret int
	intVar(&ret, -1, "retries on 429/503; 0 disables", "r", "retries")
	var chk int
	intVar(&chk, -1, "split/resume chunk size in bytes", "c", "chunk")
	var noProg bool
	boolVar(&noProg, false, "disable the live download progress line", "n", "no-progress")
	var extract string
	strVar(&extract, "", "unpack the downloaded tar/tar.gz into this dir", "x", "extract")
	flag.Usage = usage
	posArgs := parseArgs()

	p := loadPrefs(configPath(), ".env")
	if par >= 0 {
		p.Parallel = par
	}
	if wasSet("q", "quic") {
		p.Quic = useQuic
	}
	if ret >= 0 {
		p.Retries = ret
	}
	if chk > 0 {
		p.ChunkBytes = chk
	}
	if noProg {
		p.Progress = false
	}

	url := "example.com"
	if len(posArgs) > 0 {
		url = posArgs[0]
	}
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
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}

	rp := p.retry()
	start := time.Now()
	resp, err := doRetry(client, req, rp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

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
	// One progress bar for the whole run: it shows [DL] while downloading,
	// then reset()s to [XT] for extraction, reusing the same line.
	prog := newProgress(resp.ContentLength, "DL", p.Progress)
	prog.run()
	switch {
	// Split only when it can pay off and offset-writes are possible; that
	// path also owns the output file and handles resume.
	case out != "" && p.Parallel > 1 && resp.ContentLength >= int64(p.ParallelMin) && rangeable(resp):
		resp.Body.Close() // drop the probe stream; range requests refetch
		n, err = saveSplit(client, resp, out, p, rp, prog)
	case out != "":
		n, err = saveStream(out, resp.Body, prog)
	default:
		// No -o: stream the body to stdout so a bare fetch is pipeable
		// (gp URL | tar xz). All diagnostics go to stderr so stdout stays
		// byte-clean for a redirect or pipe.
		n, err = io.Copy(progWriter{os.Stdout, prog}, resp.Body)
	}
	if err != nil {
		prog.finish()
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	elapsed := time.Since(start) // download time, reported below

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
