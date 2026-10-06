#!/bin/sh
# git/git-style e2e for gp. Black-box: build the binary, exec it against
# local servers (serve.py stands in for Go's httptest -- plain HTTP and
# self-signed TLS), assert on output and saved bytes. Hermetic, no network.
#
#   sh t/e2e.sh
#
# Each test runs in its own subshell + temp dir, so exported env vars and
# written config never leak between tests (the Go suite got this from a
# fresh TempDir per test). XDG_CONFIG_HOME is pinned away from the dev's
# real ~/.config/gp; ALLOW_INSECURE/ALWAYS_ENCRYPT are scrubbed so a
# polluted parent shell can't flip outcomes.

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GP="$ROOT/out/gp"
SERVE="$ROOT/t/serve.py"

# --- minimal test harness (git test-lib idiom) -----------------------
test_count=0
test_failed=0
test_prereqs=" "
LOG=$(mktemp)
TRASH=$(mktemp -d)
CERT="$TRASH/cert.pem"
trap 'rm -rf "$LOG" "$TRASH"' EXIT

# never read the dev's real config; default to an empty XDG dir
mkdir -p "$TRASH/empty"
export XDG_CONFIG_HOME="$TRASH/empty"
unset ALLOW_INSECURE ALWAYS_ENCRYPT USER_AGENT PARALLEL PARALLEL_MIN

say() { printf '%s\n' "$*"; }
test_set_prereq() { test_prereqs="$test_prereqs$1 "; }
have_prereq() { case "$test_prereqs" in *" $1 "*) return 0 ;; esac; return 1; }

# run gp; capture combined output in $out and exit status in $code
gp() { out=$("$GP" "$@" 2>&1); code=$?; }
# fail unless $2 contains substring $1
contains() { case "$2" in *"$1"*) return 0 ;; esac; echo "missing [$1] in: $2"; return 1; }
# write gp config ($1) under a fresh XDG dir and point gp at it
write_ini() { mkdir -p xdg/gp && printf '%s' "$1" >xdg/gp/config.ini && export XDG_CONFIG_HOME="$PWD/xdg"; }

stop_server() { [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null; SERVER_PID=; }
# start serve.py ($@); set $URL and $HOST, auto-kill on subshell exit
serve() {
	python3 "$SERVE" "$@" >url 2>srv.log & SERVER_PID=$!
	trap stop_server EXIT
	until [ -s url ]; do sleep 0.02; done && read -r URL <url && HOST=${URL#*://}
}

# test_expect_success [PREREQ] 'message' 'script'
test_expect_success() {
	if [ $# -eq 3 ]; then prereq=$1; shift; else prereq=; fi
	test_count=$((test_count + 1))
	# prereq may name several space-separated tags; all must be present.
	for pr in $prereq; do
		if ! have_prereq "$pr"; then
			say "ok $test_count - $1 # SKIP (need $pr)"; return
		fi
	done
	d="$TRASH/$test_count" && mkdir -p "$d"
	if ( cd "$d" && eval "$2" ) >"$LOG" 2>&1; then
		say "ok $test_count - $1"
	else
		test_failed=$((test_failed + 1)); say "not ok $test_count - $1"; sed 's/^/#   /' "$LOG"
	fi
}

test_done() {
	say "# passed $((test_count - test_failed)) of $test_count"
	[ "$test_failed" = 0 ]
	exit
}

# --- prerequisites ---------------------------------------------------
command -v python3 >/dev/null 2>&1 || { echo "python3 required"; exit 1; }
go build -C "$ROOT" -o out/ . || { echo "build failed"; exit 1; }
openssl req -x509 -newkey rsa:2048 -keyout "$CERT" -out "$CERT" \
	-days 1 -nodes -subj /CN=localhost >/dev/null 2>&1 && test_set_prereq TLS
command -v tar >/dev/null 2>&1 && test_set_prereq TAR
command -v zstd >/dev/null 2>&1 && test_set_prereq ZSTD
command -v xz >/dev/null 2>&1 && test_set_prereq XZ
command -v bzip2 >/dev/null 2>&1 && test_set_prereq BZIP2
command -v gzip >/dev/null 2>&1 && test_set_prereq GZIP
command -v brotli >/dev/null 2>&1 && test_set_prereq BROTLI

# --- config: User-Agent ----------------------------------------------
test_expect_success 'User-Agent comes from ini [user]' '
	serve http &&
	write_ini "[user]
user-agent = gp/1.0
" &&
	gp "$URL" && test "$code" = 0 &&
	test "$(cat "$d/ua")" = gp/1.0
'

test_expect_success 'USER_AGENT env overrides ini' '
	serve http &&
	write_ini "[user]
user-agent = gp/1.0
" &&
	export USER_AGENT=from-env && gp "$URL" && test "$code" = 0 &&
	test "$(cat "$d/ua")" = from-env
'

# Full precedence chain (real env > .env > ini > default), the order loadPrefs
# resolves -- here observed through the served User-Agent.
test_expect_success 'config precedence: .env over ini, real env over .env' '
	serve http &&
	write_ini "[user]
user-agent = from-ini
" &&
	printf "USER_AGENT=from-dotenv\n" >.env &&
	gp "$URL" && test "$code" = 0 && test "$(cat "$d/ua")" = from-dotenv &&
	export USER_AGENT=from-env && gp "$URL" && test "$code" = 0 &&
	test "$(cat "$d/ua")" = from-env
'

# --- scheme / always-encrypt -----------------------------------------
test_expect_success 'plain HTTP fetch reports 200 OK' '
	serve http && gp "$URL" &&
	test "$code" = 0 && contains "200 OK" "$out"
'

test_expect_success 'scheme-less URL defaults to https://' '
	serve http && gp "$HOST" &&
	test "$code" != 0 && contains "https://$HOST" "$out"
'

test_expect_success 'always-encrypt=false (ini) keeps http://' '
	serve http &&
	write_ini "[pref]
always-encrypt = false
" &&
	gp "$HOST" && test "$code" = 0 && contains "http://$HOST" "$out"
'

test_expect_success 'ALWAYS_ENCRYPT=0 (env) keeps http://' '
	serve http &&
	export ALWAYS_ENCRYPT=0 && gp "$HOST" &&
	test "$code" = 0 && contains "http://$HOST" "$out"
'

# --- output flag -----------------------------------------------------
test_expect_success '-o creates nested dirs and saves the body' '
	serve http && gp -o nested/dir/body.txt "$URL" &&
	test "$code" = 0 && test "$(cat nested/dir/body.txt)" = hi
'

# No -o: the body goes to stdout (byte-clean, pipeable) and the summary to
# stderr, so a redirect captures only the payload and nothing is saved.
test_expect_success 'no -o streams the body to stdout, summary to stderr' '
	serve http && "$GP" "$URL/pkg/thing.bin" >body 2>err &&
	test "$(cat body)" = hi && contains "200 OK" "$(cat err)" &&
	! test -e thing.bin
'

test_expect_success '-o works after the URL, not just before it' '
	serve http && gp "$URL" -o after.txt &&
	test "$code" = 0 && test "$(cat after.txt)" = hi
'

# --- TLS verification ------------------------------------------------
test_expect_success TLS 'self-signed TLS is rejected by default' '
	serve tls "$CERT" && gp "$URL" &&
	test "$code" != 0 && contains certificate "$out"
'

test_expect_success TLS 'ALLOW_INSECURE=true (env) accepts self-signed' '
	serve tls "$CERT" &&
	export ALLOW_INSECURE=true && gp "$URL" &&
	test "$code" = 0 && contains "200 OK" "$out"
'

test_expect_success TLS 'allow-insecure=true (ini) accepts self-signed' '
	serve tls "$CERT" &&
	write_ini "[pref]
# comment line
allow-insecure = true
" &&
	gp "$URL" && test "$code" = 0 && contains "200 OK" "$out"
'

test_expect_success TLS 'ALLOW_INSECURE=1 (.env) accepts self-signed' '
	serve tls "$CERT" &&
	printf "ALLOW_INSECURE=1\n" >.env &&
	gp "$URL" && test "$code" = 0 && contains "200 OK" "$out"
'

# --- parallel range download -----------------------------------------
# serve.py: SERVE_SIZE advertises Accept-Ranges; EXPECT barriers N coexisting
# range requests; it records peak concurrency in the "maxconc" file. The new
# download path splits by CHUNK_BYTES, so to open N connections the body must
# be at least N chunks: chunk = size/N forces exactly N concurrent ranges.
test_expect_success 'parallel split reassembles and opens N connections' '
	export SERVE_SIZE=$((256 * 1024)) EXPECT=4 CHUNK_BYTES=$((64 * 1024)) && serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	export PARALLEL=4 PARALLEL_MIN=1 && gp -o out.bin "$URL" && test "$code" = 0 &&
	cmp ref.bin out.bin && test "$(cat maxconc)" -ge 4
'

test_expect_success 'small body stays single-stream (below parallel-min)' '
	export SERVE_SIZE=4096 && serve http &&
	export PARALLEL=4 PARALLEL_MIN=1048576 && gp -o out.bin "$URL" &&
	test "$code" = 0 && test "$(cat maxconc)" = 1
'

test_expect_success 'PARALLEL=1 disables splitting' '
	export SERVE_SIZE=$((256 * 1024)) && serve http &&
	export PARALLEL=1 PARALLEL_MIN=1 && gp -o out.bin "$URL" &&
	test "$code" = 0 && test "$(cat maxconc)" = 1
'

# --- chunked / resumable download ------------------------------------
# serve.py: SERVE_ETAG advertises a validator so gp writes a .gp-part
# manifest; the "ranges" file records every byte range it served, so a test
# can assert exactly which chunks were (re)fetched. A 256 KiB body at a
# 64 KiB chunk plans 4 chunks: [0-65535] [65536-131071] [131072-196607]
# [196608-262143].

test_expect_success '-c chunk size splits into ceil(size/chunk) ranges' '
	export SERVE_SIZE=$((200 * 1024)) SERVE_ETAG=v1 && serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 && gp -c $((64 * 1024)) -o out.bin "$URL" &&
	test "$code" = 0 && cmp ref.bin out.bin &&
	test "$(sort -u ranges | wc -l | tr -d " ")" = 4 &&
	test ! -e out.bin.gp-part   # manifest dropped once complete
'

# Without an explicit -c, the split auto-sizes each span to size/conns so every
# connection streams one continuous range (no mid-transfer RTT stalls). Here
# chunk-bytes is only a floor (16K < 64K = size/conns), so it must NOT force 16
# tiny ranges the way an explicit -c would.
test_expect_success 'split without -c auto-sizes to one span per connection' '
	export SERVE_SIZE=$((256 * 1024)) && serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 CHUNK_BYTES=$((16 * 1024)) &&
	gp -o out.bin "$URL" &&
	test "$code" = 0 && cmp ref.bin out.bin &&
	test "$(sort -u ranges | wc -l | tr -d " ")" = 4
'

# The flag pins the span exactly: same body and conns as above, but -c 16K now
# overrides the auto-size and yields 256K/16K = 16 ranges (finer resume, the
# user's explicit choice). Proves the flag beats the env-set floor.
test_expect_success 'explicit -c overrides auto-size and pins the span' '
	export SERVE_SIZE=$((256 * 1024)) && serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 CHUNK_BYTES=$((16 * 1024)) &&
	gp -c $((16 * 1024)) -o out.bin "$URL" &&
	test "$code" = 0 && cmp ref.bin out.bin &&
	test "$(sort -u ranges | wc -l | tr -d " ")" = 16
'

# A killed transfer leaves a manifest marking the chunks already on disk;
# the next run must refetch ONLY the gaps, trusting the bytes it kept.
test_expect_success 'resume refetches only the missing chunks' '
	export SERVE_SIZE=$((256 * 1024)) SERVE_ETAG=v1 CHUNK_BYTES=$((64 * 1024)) &&
	serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	cp ref.bin out.bin &&   # bytes for the done chunks are already correct
	printf "%s" "{\"size\":262144,\"validator\":\"v1\",\"chunk\":65536,\"done\":[true,false,true,false]}" >out.bin.gp-part &&
	rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 && gp -o out.bin "$URL" &&
	test "$code" = 0 && cmp ref.bin out.bin &&
	printf "%s\n" "65536-131071" "196608-262143" | sort >expect &&
	sort -u ranges >got && diff expect got &&
	test ! -e out.bin.gp-part
'

# A manifest whose validator no longer matches the remote is stale: gp must
# discard the on-disk bytes and refetch every chunk, not stitch onto a file
# that changed underneath it.
test_expect_success 'resume restarts when the validator changed' '
	export SERVE_SIZE=$((256 * 1024)) SERVE_ETAG=v2 CHUNK_BYTES=$((64 * 1024)) &&
	serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	head -c 262144 /dev/zero >out.bin &&   # stale garbage from a changed remote
	printf "%s" "{\"size\":262144,\"validator\":\"v1\",\"chunk\":65536,\"done\":[true,true,true,true]}" >out.bin.gp-part &&
	rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 && gp -o out.bin "$URL" &&
	test "$code" = 0 && cmp ref.bin out.bin &&
	test "$(sort -u ranges | wc -l | tr -d " ")" = 4 &&
	test ! -e out.bin.gp-part
'

# --- conditional-request cache (304 on an unchanged second run) ------
# First download records the validator in a .gp-meta sidecar; the second run
# replays it as If-None-Match, the server answers 304, and gp skips the
# transfer entirely (curl re-downloads by default).
test_expect_success 'second run is skipped via 304 when the file is unchanged' '
	export SERVE_SIZE=4096 SERVE_ETAG=v1 && serve http &&
	gp -o out.bin "$URL" && test "$code" = 0 && test -f out.bin.gp-meta &&
	gp -o out.bin "$URL" && test "$code" = 0 && contains "304 Not Modified" "$out"
'

# --force ignores the cache and refetches even when it is still current.
test_expect_success 'force re-downloads despite a fresh cache' '
	export SERVE_SIZE=4096 SERVE_ETAG=v1 && serve http &&
	gp -o out.bin "$URL" && test "$code" = 0 &&
	gp -f -o out.bin "$URL" && test "$code" = 0 && contains "200 OK" "$out"
'

# The cache is keyed to the URL it was written for: a different source URL to
# the same path must not trigger a false 304.
test_expect_success 'cache is not reused for a different URL' '
	export SERVE_SIZE=4096 SERVE_ETAG=v1 && serve http &&
	gp -o out.bin "$URL/a" && test "$code" = 0 &&
	gp -o out.bin "$URL/b" && test "$code" = 0 && contains "200 OK" "$out"
'

# A file cut short after a good run (e.g. a killed --force) keeps a valid
# meta; the size guard must refetch instead of answering "cached".
test_expect_success 'a truncated file is refetched, not served from cache' '
	export SERVE_SIZE=4096 SERVE_ETAG=v1 && serve http &&
	gp -o out.bin "$URL" && test "$code" = 0 &&
	head -c 100 out.bin >cut && mv cut out.bin &&
	gp -o out.bin "$URL" && test "$code" = 0 && contains "200 OK" "$out" &&
	test "$(wc -c <out.bin)" = 4096
'

# A pending resume (a .gp-part manifest on disk) must suppress the conditional:
# the local file is incomplete, so a 304 "you already have it" would be wrong.
test_expect_success 'a pending resume suppresses the conditional request' '
	export SERVE_SIZE=$((256 * 1024)) SERVE_ETAG=v1 CHUNK_BYTES=$((64 * 1024)) &&
	serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null &&
	cp ref.bin out.bin &&
	printf "%s" "{\"size\":262144,\"validator\":\"v1\",\"chunk\":65536,\"done\":[true,false,true,false]}" >out.bin.gp-part &&
	printf "%s" "{\"url\":\"$URL\",\"size\":262144,\"etag\":\"v1\"}" >out.bin.gp-meta &&
	export PARALLEL=4 PARALLEL_MIN=1 && gp -o out.bin "$URL" &&
	test "$code" = 0 && contains "200 OK" "$out" && cmp ref.bin out.bin
'

# --- argument / failure hygiene --------------------------------------
# A missing URL must error with usage, never fall back to a default fetch:
# this is what catches "gp -x URL" (where -x swallows the URL as its dest).
test_expect_success 'no URL given errors with usage, no fetch' '
	gp -o out.bin; test "$code" != 0 &&
	contains "no URL" "$out" && ! test -e out.bin
'

# An error status must fail before touching disk: a 404/500 page must never
# replace a good file at -o.
test_expect_success 'an error status fails and leaves an existing -o file alone' '
	echo good >keep.txt &&
	export SERVE_STATUS=404 && serve http &&
	gp -o keep.txt "$URL"; test "$code" != 0 &&
	contains "404" "$out" && test "$(cat keep.txt)" = good
'

# A failure after staging must still remove the -x temp archive (cleanup runs
# before exit, not per exit path).
test_expect_success '-x failure leaves no temp archive in $TMPDIR' '
	mkdir mytmp && export TMPDIR="$PWD/mytmp" SERVE_SIZE=1024 && serve http &&
	gp "$URL" -x dest; test "$code" != 0 &&
	contains "extract" "$out" && test -z "$(ls -A mytmp)"
'

# A non-tar payload must fail extraction WITHOUT creating the dest tree, so a
# botched run leaves nothing behind.
test_expect_success 'extracting a non-tar body leaves no dest dir' '
	export SERVE_SIZE=1024 && serve http &&   # 1KB of non-tar bytes
	gp "$URL" -x dest; test "$code" != 0 &&
	contains "extract" "$out" && ! test -e dest
'

# --- --compress: decode transfer-encoded responses ------------------
# With -z gp advertises zstd/br/gzip and inflates the body itself (no auto-gzip
# from Go's transport). The server serves the pre-compressed file verbatim
# under a Content-Encoding header; gp must reproduce the original plaintext.
test_expect_success GZIP '-z inflates a gzip transfer encoding' '
	echo "hello compress" >plain.txt && gzip -c plain.txt >body &&
	export SERVE_FILE="$PWD/body" SERVE_ENCODE=gzip && serve http &&
	gp -z -o out.txt "$URL" && test "$code" = 0 && cmp plain.txt out.txt
'

test_expect_success ZSTD '-z inflates a zstd transfer encoding' '
	echo "hello compress" >plain.txt && zstd -q -c plain.txt >body &&
	export SERVE_FILE="$PWD/body" SERVE_ENCODE=zstd && serve http &&
	gp -z -o out.txt "$URL" && test "$code" = 0 && cmp plain.txt out.txt
'

test_expect_success BROTLI '-z inflates a br (brotli) transfer encoding' '
	echo "hello compress" >plain.txt && brotli -c plain.txt >body &&
	export SERVE_FILE="$PWD/body" SERVE_ENCODE=br && serve http &&
	gp -z -o out.txt "$URL" && test "$code" = 0 && cmp plain.txt out.txt
'

# deflate is deliberately unsupported (ambiguous coding); a server that sends
# it anyway must produce a clear error, never a silently corrupt file.
test_expect_success '-z errors clearly on an unsupported deflate encoding' '
	echo data >body &&
	export SERVE_FILE="$PWD/body" SERVE_ENCODE=deflate && serve http &&
	gp -z -o out.txt "$URL"; test "$code" != 0 && contains "deflate" "$out"
'

# Without -z the default stays identity: no Accept-Encoding is sent, so the
# split path still engages on a ranged body (compression never interferes).
test_expect_success '-z off keeps identity and still splits' '
	export SERVE_SIZE=$((256 * 1024)) CHUNK_BYTES=$((64 * 1024)) && serve http &&
	"$GP" -o ref.bin "$URL" >/dev/null && rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 && gp -o out.bin "$URL" &&
	test "$code" = 0 && cmp ref.bin out.bin &&
	test "$(sort -u ranges | wc -l | tr -d " ")" = 4
'

# --- archive extraction ----------------------------------------------
test_expect_success TAR '-x unpacks a downloaded tar.gz' '
	mkdir src && echo hello >src/file.txt && mkdir src/sub && echo deep >src/sub/n.txt &&
	tar czf payload.tgz -C src . &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 &&
	test "$(cat dest/file.txt)" = hello && test "$(cat dest/sub/n.txt)" = deep
'

# A non-canonical dest ("./dest") must clean to the same root as the
# archive entries, or the traversal guard wrongly rejects every entry.
test_expect_success TAR '-x accepts a non-canonical dest path' '
	echo hi >a.txt && tar czf payload.tgz a.txt &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	gp "$URL" -x ./dest && test "$code" = 0 &&
	test "$(cat dest/a.txt)" = hi
'

test_expect_success TAR '-x without -o leaves no archive behind' '
	mkdir src && echo hi >src/a.txt && tar czf payload.tgz -C src . &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 &&
	test "$(cat dest/a.txt)" = hi && ! ls ./*.tar* >/dev/null 2>&1
'

# Combination: -x WITH -o extracts AND keeps the archive at the -o path
# (the inverse of the temp-file case above).
test_expect_success TAR '-x with -o extracts and keeps the archive' '
	mkdir src && echo hello >src/file.txt && tar czf payload.tgz -C src . &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	gp "$URL" -o kept.tgz -x dest && test "$code" = 0 &&
	test "$(cat dest/file.txt)" = hello && test -s kept.tgz
'

# Combination: parallel split + -x. A plain (uncompressed) tar keeps the body
# large enough to span several chunks, so the split path actually engages;
# extractTarGz auto-detects tar vs tar.gz, so the reassembled archive unpacks.
test_expect_success TAR 'parallel split then -x extracts the reassembled archive' '
	mkdir src && head -c 20000 /dev/zero | tr "\0" a >src/big.txt &&
	tar cf payload.tar -C src . &&
	export SERVE_FILE="$PWD/payload.tar" && serve http &&
	rm -f ranges &&
	export PARALLEL=4 PARALLEL_MIN=1 CHUNK_BYTES=4096 &&
	gp "$URL" -o arc.tar -x dest && test "$code" = 0 &&
	test "$(wc -c <dest/big.txt | tr -d " ")" = 20000 &&
	test "$(sort -u ranges | wc -l | tr -d " ")" -ge 4
'

# Codec is detected by magic bytes, not the filename, so each compressed tar
# unpacks the same as a plain one. One test per codec, skipped if the matching
# compressor is absent on the box.
test_expect_success "TAR ZSTD" '-x unpacks a tar.zst (magic-detected)' '
	mkdir src && echo zhello >src/file.txt && tar cf - -C src . | zstd -q -o payload.tar.zst &&
	export SERVE_FILE="$PWD/payload.tar.zst" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 && test "$(cat dest/file.txt)" = zhello
'

test_expect_success "TAR XZ" '-x unpacks a tar.xz (magic-detected)' '
	mkdir src && echo xhello >src/file.txt && tar cf - -C src . | xz -q >payload.tar.xz &&
	export SERVE_FILE="$PWD/payload.tar.xz" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 && test "$(cat dest/file.txt)" = xhello
'

test_expect_success "TAR BZIP2" '-x unpacks a tar.bz2 (magic-detected)' '
	mkdir src && echo bhello >src/file.txt && tar cf - -C src . | bzip2 >payload.tar.bz2 &&
	export SERVE_FILE="$PWD/payload.tar.bz2" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 && test "$(cat dest/file.txt)" = bhello
'

test_expect_success TAR '-x stages the temp archive under $TMPDIR and cleans it' '
	echo hi >a.txt && tar czf payload.tgz a.txt && mkdir mytmp &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	export TMPDIR="$PWD/mytmp" && gp "$URL" -x dest && test "$code" = 0 &&
	test "$(cat dest/a.txt)" = hi && test -z "$(ls -A mytmp)"
'

test_expect_success TAR '-x fails when $TMPDIR is unwritable (proves it is used)' '
	echo hi >a.txt && tar czf payload.tgz a.txt &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	export TMPDIR="$PWD/nope/missing" && gp "$URL" -x dest; test "$code" != 0 &&
	contains "$TMPDIR" "$out"
'

test_expect_success TAR '-x rejects path traversal (../ escape)' '
	printf evil >evil &&
	tar cf bad.tar --transform "s,^evil,../escaped," evil &&
	export SERVE_FILE="$PWD/bad.tar" && serve http &&
	gp "$URL" -x dest; test "$code" != 0 &&
	contains "unsafe path" "$out" && ! test -e ../escaped
'

# An absolute (or escaping) symlink is created verbatim -- inert until written
# through -- but a later entry that resolves out of destDir via that symlink is
# refused. evil -> abs dir, then a file at evil/pwned would land outside dest.
test_expect_success TAR '-x blocks a write through an escaping symlink' '
	mkdir escape_target &&
	ln -s "$PWD/escape_target" evil &&
	echo PWNED >evil/pwned &&
	tar cf bad.tar evil evil/pwned &&
	rm -rf evil escape_target && mkdir escape_target &&
	export SERVE_FILE="$PWD/bad.tar" && serve http &&
	gp "$URL" -x dest; test "$code" != 0 &&
	contains "via a symlink" "$out" && ! test -e escape_target/pwned
'

# A rootfs tarball legitimately ships absolute symlinks (e.g. Arch bootstrap
# var/lib/dbus/machine-id -> /etc/machine-id). Those must extract as-is, since
# nothing writes through them; the rest of the archive unpacks normally.
# A symlink entry followed by a regular file of the same name: writing the
# file must replace the link, never follow it out of dest.
test_expect_success TAR '-x replaces a same-name symlink, never writes through it' '
	mkdir escape src && ln -s "$PWD/escape/pwned" src/a &&
	tar cf bad.tar -C src a &&
	rm src/a && echo PWNED >src/a && tar rf bad.tar -C src a &&
	export SERVE_FILE="$PWD/bad.tar" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 &&
	! test -e escape/pwned && ! test -L dest/a && test "$(cat dest/a)" = PWNED
'

test_expect_success TAR '-x extracts an inert absolute symlink verbatim' '
	ln -s /etc/somewhere link && mkdir realdir && echo hi >realdir/f.txt &&
	tar cf ok.tar link realdir &&
	export SERVE_FILE="$PWD/ok.tar" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 &&
	test "$(readlink dest/link)" = /etc/somewhere &&
	test "$(cat dest/realdir/f.txt)" = hi
'

test_done
