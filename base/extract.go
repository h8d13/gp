// Tar extraction for downloaded archives. The tar payload may be raw or
// wrapped in gzip, zstd, xz, or bzip2; the codec is picked by magic bytes, not
// the filename, so a mislabeled archive still unpacks. Each wrapper is decoded
// by shelling out to the system tool (see decodeCmd), so there are no vendored
// decompressors.
//
// Two layers keep a hostile archive inside destDir (the classic "Zip Slip"):
// safeJoin rejects a lexical escape in the entry name ("../" or an absolute
// path), and within() rejects an escape through a symlink, where one entry
// makes a link pointing out of destDir and a later entry writes through it.
// Symlinks themselves may point anywhere (a rootfs tarball legitimately ships
// absolute links like /etc/machine-id); they are inert until something writes
// through them, and within() blocks exactly that.
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

// decompress streams br through the right system decompressor for the tar
// payload, chosen by leading magic bytes: gzip, zstd, xz, or bzip2; an
// unrecognized header is read as a plain (uncompressed) tar. Peek does not
// consume, so the tool reads the stream from byte 0. closeFn reaps the process
// (nil for the plain-tar case).
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

	tr := tar.NewReader(r)
	// Don't touch the filesystem until the payload proves it's a tar: destDir
	// is created lazily on the first valid entry, so a bogus body (an HTML
	// error page, a wrong file) fails on the first header read without leaving
	// an empty dir tree behind.
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

// within reports whether target stays inside destDir once the symlinks on its
// existing ancestors are resolved. safeJoin only checks the name lexically; a
// prior entry may have planted a symlink ancestor pointing out of destDir, so
// the lexical path looks contained while the real one escapes. Resolving the
// deepest existing ancestor catches that. destDir exists by the time any entry
// is written, so it always anchors the walk.
func within(destDir, target string) (bool, error) {
	root, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return false, err
	}
	if target == filepath.Clean(destDir) {
		return true, nil // the archive's own "." entry: destDir itself
	}
	// The entry is created inside target's parent, so resolve the deepest
	// existing ancestor of that parent and require it to stay under root.
	// Resolving the parent (not target) avoids tripping on a legitimate symlink
	// leaf, whose own destination is allowed to point outside.
	for p := filepath.Dir(target); ; {
		if _, err := os.Lstat(p); err == nil {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return false, err
			}
			return real == root || strings.HasPrefix(real, root+string(os.PathSeparator)), nil
		}
		parent := filepath.Dir(p)
		if parent == p { // reached the filesystem root without an existing ancestor
			return false, nil
		}
		p = parent
	}
}

// writeEntry materializes one tar entry. Directories and regular files are
// created; symlinks are created verbatim (their target may point anywhere, but
// nothing is ever written through them that escapes destDir; see within). Other
// types (devices, fifos, hardlinks) are skipped.
func writeEntry(tr *tar.Reader, hdr *tar.Header, destDir, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeSymlink:
		// Refuse to create anything whose real (symlink-resolved) location
		// has escaped destDir.
		ok, err := within(destDir, target)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("unsafe path escapes %s via a symlink: %q", destDir, hdr.Name)
		}
	}
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
		return os.Symlink(hdr.Linkname, target)
	default:
		return nil
	}
}
