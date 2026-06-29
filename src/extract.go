// Tar extraction for downloaded archives. The tar payload may be raw or
// wrapped in gzip, zstd, xz, or bzip2; the codec is picked by magic bytes, not
// the filename, so a mislabeled archive still unpacks. gzip and bzip2 are
// stdlib; zstd and xz are pure-Go (no cgo) third-party readers. Every entry
// path is validated to stay within destDir, so a hostile archive cannot escape
// via "../" or an absolute path or a symlink (the classic "Zip Slip").
package src

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// decompress wraps br in the right decompressor for the tar payload, chosen by
// leading magic bytes: gzip, zstd, xz, or bzip2; an unrecognized header is read
// as a plain (uncompressed) tar. Peek does not consume, so the returned reader
// still starts at byte 0. closeFn releases the codec (nil when none needs it).
func decompress(br *bufio.Reader) (io.Reader, func() error, error) {
	// 6 bytes covers the longest signature (xz). Short reads (a tiny archive)
	// just fall through the length guards to the plain-tar default.
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
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return gz, gz.Close, nil
	case has(0x28, 0xb5, 0x2f, 0xfd): // zstd
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return zr, func() error { zr.Close(); return nil }, nil
	case has(0xfd, '7', 'z', 'X', 'Z', 0x00): // xz
		xr, err := xz.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return xr, nil, nil
	case has('B', 'Z', 'h'): // bzip2
		return bzip2.NewReader(br), nil, nil
	default:
		return br, nil, nil
	}
}

// extractTarGz unpacks the archive at src into destDir and returns the number
// of entries written. Any supported compression wrapper is decoded on the fly
// (see decompress); a plain tar is read directly.
func extractTarGz(src, destDir string, prog *progress) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Count bytes pulled from the archive file, so prog fills 0->100% as the
	// stream is consumed (the decompressor reads compressed bytes through this).
	br := bufio.NewReader(progReader{f, prog})
	r, closeFn, err := decompress(br)
	if err != nil {
		return 0, err
	}
	if closeFn != nil {
		defer closeFn()
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}
	tr := tar.NewReader(r)
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
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
}

// safeJoin resolves name under dir and rejects any result that escapes
// dir (path traversal). Returns the cleaned absolute-within-dir target.
func safeJoin(dir, name string) (string, error) {
	dir = filepath.Clean(dir)
	target := filepath.Join(dir, name)
	if target != dir && !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return target, nil
}

// writeEntry materializes one tar entry. Directories and regular files
// are created; symlinks are created only when their resolved target stays
// inside destDir; other types (devices, fifos, hardlinks) are skipped.
func writeEntry(tr *tar.Reader, hdr *tar.Header, destDir, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, tr)
		return err
	case tar.TypeSymlink:
		if _, err := safeJoin(destDir, filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)); err != nil {
			return err
		}
		return os.Symlink(hdr.Linkname, target)
	default:
		return nil
	}
}
