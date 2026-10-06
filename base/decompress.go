// Decompression shells out to system tools (see ./deps): faster than the
// pure-Go ports (xz most of all) and keeps go.mod to the HTTP/3 stack.
package base

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// The closer kills the process, so a reader that stops early can't deadlock
// on a full pipe; after a full read it's a no-op and Wait reports the real
// status.
func decodeCmd(tool string, in io.Reader) (io.Reader, func() error, error) {
	if _, err := exec.LookPath(tool); err != nil {
		return nil, nil, fmt.Errorf("%s not found on PATH "+
			"(gp shells out to decompress; install it)", tool)
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
