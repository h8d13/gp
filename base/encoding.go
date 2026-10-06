// --compress trades identity's Content-Length, ranges and ETag (see
// newClient) for fewer wire bytes. Only codings gp decodes are advertised;
// deflate is ambiguous in the wild (raw RFC 1951 vs zlib RFC 1950), so it is
// refused rather than guessed.
package base

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// strongest ratio first
const acceptEncoding = "zstd, br, gzip"

func isIdentity(enc string) bool {
	return enc == "" || strings.EqualFold(enc, "identity")
}

// Unknown codings error out rather than write a corrupt file.
func decodeBody(resp *http.Response) (io.Reader, func() error, error) {
	enc := resp.Header.Get("Content-Encoding")
	switch {
	case isIdentity(enc):
		return resp.Body, nil, nil
	case strings.EqualFold(enc, "gzip"):
		return decodeCmd("gzip", resp.Body)
	case strings.EqualFold(enc, "zstd"):
		return decodeCmd("zstd", resp.Body)
	case strings.EqualFold(enc, "br"):
		return decodeCmd("brotli", resp.Body)
	default:
		return nil, nil, fmt.Errorf("unsupported Content-Encoding %q "+
			"(deflate is not implemented)", enc)
	}
}
