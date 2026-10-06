// Tar extraction. Two layers keep a hostile archive inside destDir ("Zip
// Slip"): safeJoin rejects a lexical escape ("../", absolute path), within()
// rejects one through a symlink planted by an earlier entry. Symlinks may
// point anywhere (rootfs tarballs ship /etc/machine-id links); they are
// inert until written through, which within() blocks.
package base

import (
	"archive/tar"
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Codec by magic bytes, not filename, so a mislabeled archive still unpacks;
// an unknown header is a plain tar. Peek doesn't consume: the tool reads
// from byte 0.
func decompress(br *bufio.Reader) (io.Reader, func() error, error) {
	// 6 covers xz, the longest; short reads fall through to plain tar.
	magic, _ := br.Peek(6)
	has := func(sig ...byte) bool {
		if len(magic) < len(sig) {
			return false
		}
		for i, b := range sig {
			if magic[i] != b {
				return false
			}
		}
		return true
	}
	switch {
	case has(0x1f, 0x8b): // gzip
		return decodeCmd("gzip", br)
	case has(0x28, 0xb5, 0x2f, 0xfd): // zstd
		return decodeCmd("zstd", br)
	case has(0xfd, '7', 'z', 'X', 'Z', 0x00): // xz
		return decodeCmd("xz", br)
	case has('B', 'Z', 'h'): // bzip2
		return decodeCmd("bzip2", br)
	default:
		return br, nil, nil
	}
}

func extractTarGz(src, destDir string, prog *progress) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// count compressed bytes, so the bar tracks the archive file
	br := bufio.NewReader(progReader{f, prog})
	r, closeFn, err := decompress(br)
	if err != nil {
		return 0, err
	}
	if closeFn != nil {
		defer closeFn()
	}

	tr := tar.NewReader(r)
	// destDir is made on the first valid header, so a non-tar body (an
	// HTML error page) leaves no empty dir behind.
	n := 0
	made := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		if !made {
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				return n, err
			}
			made = true
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return n, err
		}
		if err := writeEntry(tr, hdr, destDir, target); err != nil {
			return n, err
		}
		n++
	}
	// A valid but empty archive still yields the (empty) dest dir.
	if !made {
		return n, os.MkdirAll(destDir, 0o755)
	}
	return n, nil
}

func safeJoin(dir, name string) (string, error) {
	dir = filepath.Clean(dir)
	target := filepath.Join(dir, name)
	if target != dir && !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return target, nil
}

// within resolves target's deepest existing ancestor and requires it under
// destDir. The parent is resolved, not target, so a symlink leaf may itself
// point outside.
func within(destDir, target string) (bool, error) {
	root, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return false, err
	}
	if target == filepath.Clean(destDir) {
		return true, nil // the archive's own "." entry: destDir itself
	}
	for p := filepath.Dir(target); ; {
		if _, err := os.Lstat(p); err == nil {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return false, err
			}
			sep := string(os.PathSeparator)
			return real == root || strings.HasPrefix(real, root+sep), nil
		}
		parent := filepath.Dir(p)
		if parent == p { // hit / without an existing ancestor
			return false, nil
		}
		p = parent
	}
}

// Devices, fifos and hardlinks are skipped.
func writeEntry(tr *tar.Reader, hdr *tar.Header, destDir, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
		ok, err := within(destDir, target)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("unsafe path escapes %s via a symlink: %q",
				destDir, hdr.Name)
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode) & 0o777
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, tr)
		return err
	case tar.TypeSymlink:
		return os.Symlink(hdr.Linkname, target)
	default:
		return nil
	}
}
