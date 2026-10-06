// Live progress on stderr, only on a TTY so pipes stay byte-clean. One
// goroutine renders; writers bump an atomic, keeping the split path
// lock-free.
package base

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// A nil *progress is a no-op, so callers never branch on TTY-ness. Atomics
// let reset() re-arm the bar while render reads.
type progress struct {
	total     atomic.Int64
	done      atomic.Int64
	startNano atomic.Int64
	label     atomic.Value // string, e.g. "DL" / "XT"
	stop      chan struct{}
	stopOnce  sync.Once
	ended     chan struct{}
}

// total 0 means unknown (indeterminate bar).
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

// reset re-arms the same bar and line for a new phase.
func (p *progress) reset(total int64, label string) {
	if p == nil {
		return
	}
	p.done.Store(0)
	p.total.Store(total)
	p.startNano.Store(time.Now().UnixNano())
	p.label.Store(label)
}

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

func (p *progress) add(n int64) {
	if p != nil {
		p.done.Add(n)
	}
}

// finish blocks until the last line is out, so the summary never
// interleaves. Idempotent: callers both defer it and call it before output.
func (p *progress) finish() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.ended
}

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
		line = fmt.Sprintf("[%s] %s  %s/s",
			label, humanBytes(done), humanBytes(speed))
	} else {
		// Shrink on narrow terminals: a wrapped line breaks the \r redraw.
		frac := float64(done) / float64(total)
		left := fmt.Sprintf("[%s] %5.1f%% [", label, frac*100)
		right := fmt.Sprintf("] %s / %s  %s/s",
			humanBytes(done), humanBytes(total), humanBytes(speed))
		bw := max(min(24, cols-runeLen(left)-runeLen(right)-1), 0)
		filled := max(0, min(bw, int(frac*float64(bw))))
		bar := strings.Repeat("▓", filled) + strings.Repeat("░", bw-filled)
		line = left + bar + right
	}
	// \x1b[K erases the tail of a longer previous line
	fmt.Fprintf(os.Stderr, "\r%s\x1b[K", clip(line, cols-1))
}

// Queried per render to follow resizes.
func termCols() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// Every glyph the bar uses is single-width, so runes = columns.
func runeLen(s string) int { return len([]rune(s)) }

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

type progWriter struct {
	w io.Writer
	p *progress
}

func (pw progWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	pw.p.add(int64(n))
	return n, err
}

type progReader struct {
	r io.Reader
	p *progress
}

func (pr progReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	pr.p.add(int64(n))
	return n, err
}

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
