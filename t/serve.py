#!/usr/bin/env python3
# Local test server: the shell harness's stand-in for Go's httptest, so the
# e2e tests stay hermetic (no network). Modes:
#
#   serve.py http              plain HTTP, body "hi"
#   serve.py tls  CERT         self-signed TLS (CERT holds key+cert), body "hi"
#
# Env knobs:
#   SERVE_SIZE  serve a deterministic pattern body of N bytes and advertise
#               Accept-Ranges: bytes, so gp's parallel split path engages.
#   EXPECT      hold every range request at a barrier until EXPECT of them
#               coexist, turning "did gp open N connections" into a hard
#               check (max concurrency) rather than a timing race.
#   SERVE_ETAG  advertise this value as the ETag (a resume validator) on
#               both the probe and every range reply, so gp persists a
#               .gp-part manifest and a later run can resume. Also answers a
#               matching If-None-Match with 304, exercising the gp cache.
#   SERVE_RELEASE  path to a JSON file served (read fresh per request, as
#               application/json) for any forge "latest release" API path --
#               i.e. one containing "releases" and ending in "latest". Lets
#               `gp up` resolve a release without the network; the file is
#               written by the test AFTER serve prints its URL, so it can
#               embed the dynamic asset download URL. The requested path is
#               appended to the "apipath" file so a test can assert the
#               forge built the right endpoint (e.g. GitLab's %2F encoding).
#   SERVE_INDEX  path to an HTML file served (as text/html) for any request
#               path ending in "/" -- a directory-index listing for an index
#               source to resolve a file from; the linked files are served by
#               the normal body path.
#   SERVE_ENCODE advertise this Content-Encoding (gzip/zstd/br) and serve the
#               already-compressed SERVE_FILE verbatim, with no Accept-Ranges,
#               to drive gp's --compress decode path.
#   SERVE_SUMS  path to a checksums file served (as text/plain) for any request
#               path that ends in ".sha256" or contains "sums", so a source's
#               sha256-url can resolve an asset's digest without the network.
#   SERVE_PKGLIST / SERVE_PKGFILES  JSON files for the Forgejo/Gitea generic
#               package API: the version list (/api/v1/packages/...) and the file
#               list (.../files). The download (/api/packages/...) is the normal
#               body path, so a `package =` source resolves and installs offline.
#   SERVE_TAGS  JSON tag list (newest first) served for any ".../tags..." path,
#               so a `tag = true` source resolves the latest tag; the archive
#               download (.../archive/...) is the normal body path.
#
# Prints the bound base URL on the first stdout line, then serves forever.
# Side-effect files written in CWD: "ua" (last User-Agent), "maxconc" (peak
# concurrent requests), "ranges" (one "lo-hi" line per range request, so a
# resume test can assert exactly which chunks were (re)fetched) -- the shell
# reads these to assert.
import http.server, ssl, sys, os, threading

mode = sys.argv[1]
SIZE = int(os.environ.get("SERVE_SIZE", "0"))
EXPECT = int(os.environ.get("EXPECT", "1"))
ETAG = os.environ.get("SERVE_ETAG", "")
# SERVE_ENCODE: advertise this Content-Encoding and serve the (already
# compressed) SERVE_FILE bytes verbatim, so gp's --compress decode path can be
# tested end to end. A compressed response carries no byte ranges.
ENCODE = os.environ.get("SERVE_ENCODE", "")


def pattern(n):
    # Non-repeating LCG stream: a chunk written to the wrong offset mismatches.
    b = bytearray(n)
    x = 0x9E3779B9
    for i in range(n):
        x = (x * 1664525 + 1013904223) & 0xFFFFFFFF
        b[i] = (x >> 24) & 0xFF
    return bytes(b)


if os.environ.get("SERVE_FILE"):
    with open(os.environ["SERVE_FILE"], "rb") as fh:
        BODY = fh.read()
elif SIZE:
    BODY = pattern(SIZE)
else:
    BODY = b"hi"
# Advertise Accept-Ranges for any sized body (a pattern or a served file), so
# gp's split path can engage on a real archive, not just the synthetic stream.
RANGES = (SIZE > 0 or bool(os.environ.get("SERVE_FILE"))) and not ENCODE
lock = threading.Lock()
barrier = threading.Barrier(EXPECT) if EXPECT > 1 else None
cur = mx = 0


def enter():
    global cur, mx
    with lock:
        cur += 1
        mx = max(mx, cur)
        open("maxconc", "w").write(str(mx))


def leave():
    global cur
    with lock:
        cur -= 1


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        enter()
        try:
            self._serve()
        except Exception:
            pass  # gp drops the probe stream early -> broken pipe is expected
        finally:
            leave()

    # HEAD answers the validator probe a bare-url source makes before deciding
    # whether to re-download: same headers as the body GET, no body.
    def do_HEAD(self):
        self.send_response(200)
        if RANGES:
            self.send_header("Accept-Ranges", "bytes")
        if ETAG:
            self.send_header("ETag", ETAG)
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()

    def _send_file(self, path, ctype):
        with open(path, "rb") as fh:
            payload = fh.read()
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _serve(self):
        open("ua", "w").write(self.headers.get("User-Agent", ""))
        # Generic package registry: the version list (/api/v1/packages/{owner}?
        # type=generic) returns SERVE_PKGLIST; the file list (.../files) returns
        # SERVE_PKGFILES; the actual download (/api/packages/...) is the normal
        # body path further down.
        pkgfiles = os.environ.get("SERVE_PKGFILES")
        if pkgfiles and "/api/v1/packages/" in self.path and self.path.endswith("/files"):
            self._send_file(pkgfiles, "application/json")
            return
        pkglist = os.environ.get("SERVE_PKGLIST")
        if pkglist and "/api/v1/packages/" in self.path:
            self._send_file(pkglist, "application/json")
            return
        # Tag list for a `tag =` source: any ".../tags..." path returns
        # SERVE_TAGS; the archive download (.../archive/...) is the normal body.
        tagsfile = os.environ.get("SERVE_TAGS")
        if tagsfile and "/tags" in self.path:
            self._send_file(tagsfile, "application/json")
            return
        # Forge release API: any ".../releases/.../latest" path returns the
        # SERVE_RELEASE JSON, read fresh so the test can write it post-bind.
        # Directory index: a path ending in "/" returns the SERVE_INDEX HTML
        # (a listing of <a href> file links) so an index source can resolve a
        # file from it; the linked files are served by the normal body path.
        idxfile = os.environ.get("SERVE_INDEX")
        if idxfile and self.path.endswith("/"):
            with open(idxfile, "rb") as fh:
                payload = fh.read()
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        sumsfile = os.environ.get("SERVE_SUMS")
        if sumsfile and (self.path.endswith(".sha256") or "sums" in self.path.lower()):
            with open(sumsfile, "rb") as fh:
                payload = fh.read()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        relfile = os.environ.get("SERVE_RELEASE")
        if relfile and "releases" in self.path and self.path.endswith("latest"):
            with lock:
                open("apipath", "a").write(self.path + "\n")
            with open(relfile, "rb") as fh:
                payload = fh.read()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        # Conditional GET: a matching If-None-Match (the validator gp cached
        # from a prior run) means the file is unchanged -> 304, no body. Only
        # the full probe carries it; range requests never do.
        if ETAG and self.headers.get("If-None-Match") == ETAG:
            self.send_response(304)
            self.send_header("ETag", ETAG)
            self.end_headers()
            return
        rng = self.headers.get("Range")
        if rng:
            lo, hi = (int(v) for v in rng.replace("bytes=", "").split("-"))
            with lock:
                open("ranges", "a").write(f"{lo}-{hi}\n")
            if barrier:
                barrier.wait()  # prove all EXPECT range requests coexist
            self.send_response(206)
            self.send_header("Accept-Ranges", "bytes")
            if ETAG:
                self.send_header("ETag", ETAG)
            self.send_header("Content-Range", f"bytes {lo}-{hi}/{len(BODY)}")
            self.send_header("Content-Length", str(hi - lo + 1))
            self.end_headers()
            self.wfile.write(BODY[lo : hi + 1])
            return
        self.send_response(200)
        if RANGES:
            self.send_header("Accept-Ranges", "bytes")
        if ENCODE:
            self.send_header("Content-Encoding", ENCODE)
        if ETAG:
            self.send_header("ETag", ETAG)
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)


httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
if mode == "tls":
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(sys.argv[2])
    httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
scheme = "https" if mode == "tls" else "http"
print(f"{scheme}://127.0.0.1:{httpd.server_address[1]}", flush=True)
httpd.serve_forever()
