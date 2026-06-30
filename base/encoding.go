// HTTP transfer-compression for the --compress path. By default gp fetches
// identity bytes (see newClient: that keeps Content-Length, byte ranges, and
// the ETag intact, so split/resume/304 work). --compress trades those away to
// save wire bytes on compressible payloads (large text/JSON), advertising the
// codings gp can actually decode and inflating the body itself.
//
// A client may only offer what it can decode: advertise br and let the server
// pick it, and without a decoder the body is garbage. So acceptEncoding lists
// exactly the four below. deflate is intentionally absent -- the HTTP "deflate"
// coding is ambiguous in the wild (raw RFC 1951 vs zlib-wrapped RFC 1950) and
// effectively unused, so it is documented as unsupported rather than guessed.
package base

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// acceptEncoding is what --compress advertises: the codings decodeBody handles
// (each decoded by shelling out to gzip/zstd/brotli; see decodeCmd). Order is
// preference, strongest ratio first.
const acceptEncoding = "zstd, br, gzip"

// isIdentity reports whether a Content-Encoding means "no decoding needed".
func isIdentity(enc string) bool {
	return enc == "" || strings.EqualFold(enc, "identity")
}

// decodeBody streams resp.Body through the decoder named by its Content-Encoding,
// returning the plaintext reader and a closer (nil when none is needed). An
// identity response passes through untouched, so the default (non-compress) path
// is unaffected. An unknown coding -- notably deflate -- is a hard error rather
// than a silently corrupt file.
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
		return nil, nil, fmt.Errorf("unsupported Content-Encoding %q (deflate is not implemented)", enc)
	}
}
