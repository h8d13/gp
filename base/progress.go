// Live download progress on stderr. Rendered only when stderr is a
// terminal, so piped or redirected runs (scripts, tests, `gp ... | foo`)
// stay byte-clean. A single goroutine owns all rendering; writers just
// bump an atomic counter, which keeps the parallel path lock-free.
package base

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// progress reports bytes transferred against an optional total. A nil
// *progress is a valid no-op, so callers never branch on TTY-ness. The
// mutable phase fields are atomic so reset() can re-arm the bar from the
// main goroutine while the render goroutine reads, without a lock.
type progress struct {
	total     atomic.Int64
	done      atomic.Int64
	startNano atomic.Int64
	label     atomic.Value // string, e.g. "DL" / "XT"
	stop      chan struct{}
	ended     chan struct{}
}

// newProgress returns a live reporter, or nil (the no-op case) when
// disabled or when stderr is not a terminal. total is the Content-Length
// (0 if unknown); label tags the current phase.
func newProgress(total int64, label string, enabled bool) *progress {
	if !enabled || !isTerminal(os.Stderr) {
		return nil
	}
	p := &progress{stop: make(chan struct{}), ended: make(chan struct{})}
	p.total.Store(total)
	p.startNano.Store(time.Now().UnixNano())
	p.label.Store(label)
	return p
}

// reset re-arms the same bar for a new phase (new total and label),
// keeping the one render goroutine and output line. Safe on nil.
func (p *progress) reset(total int64, label string) {
	if p == nil {
		return
	}
	p.done.Store(0)
	p.total.Store(total)
	p.startNano.Store(time.Now().UnixNano())
	p.label.Store(label)
}

// run starts the render loop in the background. Safe to call on nil.
func (p *progress) run() {
	if p == nil {
		return
	}
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				p.render()
				fmt.Fprintln(os.Stderr)
				close(p.ended)
				return
			case <-t.C:
				p.render()
			}
		}
	}()
}

// add records n more transferred bytes. Safe on nil and from multiple
// goroutines (the parallel path calls it from each connection).
func (p *progress) add(n int64) {
	if p != nil {
		p.done.Add(n)
	}
}

// finish stops rendering and blocks until the final line has printed, so
// the caller's summary line never interleaves with progress output.
func (p *progress) finish() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.ended
}

// render draws one in-place status line. Trailing spaces erase the tail
// of a previously longer line.
func (p *progress) render() {
	done, total := p.done.Load(), p.total.Load()
	label, _ := p.label.Load().(string)
	var speed int64
	if el := time.Since(time.Unix(0, p.startNano.Load())).Seconds(); el > 0 {
		speed = int64(float64(done) / el)
	}
	cols := termCols()
	var line string
	if total <= 0 {
		line = fmt.Sprintf("[%s] %s  %s/s", label, humanBytes(done), humanBytes(speed))
	} else {
		// Size the bar to the space left after the surrounding text, capped, so
		// it shrinks (to nothing) on a narrow terminal instead of overflowing
		// and wrapping. A wrapped line defeats the \r in-place redraw.
		frac := float64(done) / float64(total)
		left := fmt.Sprintf("[%s] %5.1f%% [", label, frac*100)
		right := fmt.Sprintf("] %s / %s  %s/s", humanBytes(done), humanBytes(total), humanBytes(speed))
		bw := max(min(24, cols-runeLen(left)-runeLen(right)-1), 0)
		filled := max(0, min(bw, int(frac*float64(bw))))
		bar := strings.Repeat("▓", filled) + strings.Repeat("░", bw-filled)
		line = left + bar + right
	}
	// Clip to the terminal and erase any tail of a previously longer line
	// (\x1b[K), so the redraw stays on one row.
	fmt.Fprintf(os.Stderr, "\r%s\x1b[K", clip(line, cols-1))
}

// termCols returns stderr's terminal width in columns, or 80 when it cannot be
// queried, so the bar always has a width to fit within. Queried each render so
// a mid-transfer resize is picked up.
func termCols() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// runeLen counts s in display columns: every glyph the bar uses (text, ▓/░, ✓)
// is single-width, so a rune count is the column count.
func runeLen(s string) int { return len([]rune(s)) }

// clip truncates s to at most n columns (rune-wise, never splitting a glyph) so
// the rendered line fits the terminal.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// progWriter forwards writes to w while reporting their length to p.
type progWriter struct {
	w io.Writer
	p *progress
}

func (pw progWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	pw.p.add(int64(n))
	return n, err
}

// progReader forwards reads from r while reporting their length to p, so
// the same bar can track consumption of an input stream (e.g. unpacking
// an archive) as well as a download.
type progReader struct {
	r io.Reader
	p *progress
}

func (pr progReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	pr.p.add(int64(n))
	return n, err
}

// isTerminal reports whether f is a character device (a TTY), so progress
// never corrupts a pipe or a regular file.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// humanBytes formats n as a 1024-based size, e.g. 184234252 -> "175.7 MiB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
