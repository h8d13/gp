// External decompression. gp shells out to the system tools (xz, zstd, gzip,
// bzip2, brotli) rather than vendoring Go reimplementations: those tools ship
// with any Arch/Linux base, decode faster than the pure-Go ports (xz most of
// all), and keep go.mod down to the HTTP/3 stack. Both the tar-extract path
// (extract.go) and the --compress transfer path (encoding.go) stream through
// here. The required binaries are listed in ./deps.
package base

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// decodeCmd streams in through `tool -dc` (decompress to stdout), returning the
// plaintext reader and a closer that reaps the process. A missing tool is a
// clear error up front. The closer cancels (kills) the process, so a caller
// that stops reading early -- a mid-extract failure -- never deadlocks on a
// full stdout pipe; a normal full read leaves the process already exited, so
// the cancel is a no-op and Wait reports its real status.
func decodeCmd(tool string, in io.Reader) (io.Reader, func() error, error) {
	if _, err := exec.LookPath(tool); err != nil {
		return nil, nil, fmt.Errorf("%s not found on PATH (gp shells out to decompress; install it)", tool)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, tool, "-dc")
	cmd.Stdin = in
	cmd.Stderr = os.Stderr // surface the tool's own message on a corrupt stream
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, err
	}
	return out, func() error { cancel(); return cmd.Wait() }, nil
}
