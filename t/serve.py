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
#   SERVE_STATUS answer every GET with this status and a short error body,
#               so failure handling (no clobbered -o file) can be tested.
#   SERVE_ENCODE advertise this Content-Encoding (gzip/zstd/br) and serve the
#               already-compressed SERVE_FILE verbatim, with no Accept-Ranges,
#               to drive gp's --compress decode path.
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
STATUS = int(os.environ.get("SERVE_STATUS", "0"))


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

    def _serve(self):
        open("ua", "w").write(self.headers.get("User-Agent", ""))
        if STATUS:
            page = b"error page"
            self.send_response(STATUS)
            self.send_header("Content-Length", str(len(page)))
            self.end_headers()
            self.wfile.write(page)
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
