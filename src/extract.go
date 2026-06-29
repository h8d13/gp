// Tar extraction for downloaded archives. Stdlib only (archive/tar +
// compress/gzip), gzip auto-detected by magic bytes so plain .tar and
// gzipped .tar.gz/.tgz both work. Every entry path is validated to stay
// within destDir, so a hostile archive cannot escape via "../" or an
// absolute path or a symlink (the classic "Zip Slip").
package src

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTarGz unpacks the archive at src into destDir and returns the
// number of entries written. A gzip-compressed archive is gunzipped on
// the fly; a plain tar is read directly.
func extractTarGz(src, destDir string, prog *progress) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Count bytes pulled from the archive file, so prog fills 0->100% as
	// the stream is consumed (gzip reads the compressed bytes through this).
	br := bufio.NewReader(progReader{f, prog})
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		r = gz
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
