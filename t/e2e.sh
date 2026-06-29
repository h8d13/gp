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
write_ini() { mkdir -p xdg/gp && printf '%s' "$1" >xdg/gp/gpconfig.ini && export XDG_CONFIG_HOME="$PWD/xdg"; }
# write sources.ini ($1) under a fresh XDG dir; LOCK points at its sidecar lock
write_sources() { mkdir -p xdg/gp && printf '%s' "$1" >xdg/gp/sources.ini && export XDG_CONFIG_HOME="$PWD/xdg" LOCK="$PWD/xdg/gp/sources.lock"; }

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
	if [ -n "$prereq" ] && ! have_prereq "$prereq"; then
		say "ok $test_count - $1 # SKIP (need $prereq)"; return
	fi
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

test_expect_success 'bare URL saves to a derived filename (no -o)' '
	serve http && gp "$URL/pkg/thing.bin" &&
	test "$code" = 0 && test "$(cat thing.bin)" = hi
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

# --- archive extraction ----------------------------------------------
test_expect_success TAR '-x unpacks a downloaded tar.gz' '
	mkdir src && echo hello >src/file.txt && mkdir src/sub && echo deep >src/sub/n.txt &&
	tar czf payload.tgz -C src . &&
	export SERVE_FILE="$PWD/payload.tgz" && serve http &&
	gp "$URL" -x dest && test "$code" = 0 &&
	test "$(cat dest/file.txt)" = hello && test "$(cat dest/sub/n.txt)" = deep
'

test_expect_success 'a second positional arg saves to that file (implies -o)' '
	serve http && gp "$URL" saved.txt &&
	test "$code" = 0 && test "$(cat saved.txt)" = hi
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

# --- up: forge release sync ------------------------------------------
# serve.py SERVE_RELEASE returns a release JSON for the forge "latest" API
# path (read fresh, so the test embeds the dynamic asset URL after bind), and
# serves the SERVE_FILE archive as the asset body. host=$URL forces gp's forge
# adapter at the local server over http (forgeBase honors an explicit scheme).
# Tag-vs-lock comparison is gp's whole version check, so these exercise both
# the fetch/extract path and the install-only-when-newer logic.

# helper: write a github/gitea-shaped release JSON for asset name $1 at the
# served asset URL, tagged $2, into rel.json
gh_release() { printf '{"tag_name":"%s","assets":[{"name":"%s","browser_download_url":"%s/dl/%s","size":1}]}' "$2" "$1" "$URL" "$1" >rel.json; }

test_expect_success TAR 'up installs a github release, extracts it, records the tag' '
	mkdir srcd && echo hello >srcd/file.txt && tar czf app.tgz -C srcd . &&
	export SERVE_FILE="$PWD/app.tgz" SERVE_RELEASE="$PWD/rel.json" && serve http &&
	gh_release tool-linux-amd64.tar.gz v1.0.0 &&
	write_sources "[TOOL]
host = $URL
repo = owner/tool
match = linux-amd64
ext = tar.gz
dest = $PWD/inst
" &&
	gp up && test "$code" = 0 && contains "installing v1.0.0" "$out" &&
	test "$(cat inst/file.txt)" = hello &&
	grep -q "TOOL = v1.0.0" "$LOCK"
'

test_expect_success TAR 'up is a no-op when the lock tag matches the release' '
	mkdir srcd && echo hi >srcd/f.txt && tar czf app.tgz -C srcd . &&
	export SERVE_FILE="$PWD/app.tgz" SERVE_RELEASE="$PWD/rel.json" && serve http &&
	gh_release tool-linux-amd64.tar.gz v1.0.0 &&
	write_sources "[TOOL]
host = $URL
repo = owner/tool
match = linux-amd64
ext = tar.gz
dest = $PWD/inst
" &&
	gp up && test "$code" = 0 &&
	gp up && test "$code" = 0 && contains "up to date (v1.0.0)" "$out"
'

test_expect_success TAR 'up updates when the release tag is newer than the lock' '
	mkdir srcd && echo hi >srcd/f.txt && tar czf app.tgz -C srcd . &&
	export SERVE_FILE="$PWD/app.tgz" SERVE_RELEASE="$PWD/rel.json" && serve http &&
	write_sources "[TOOL]
host = $URL
repo = owner/tool
match = linux-amd64
ext = tar.gz
dest = $PWD/inst
" &&
	gh_release tool-linux-amd64.tar.gz v1.0.0 && gp up && test "$code" = 0 &&
	gh_release tool-linux-amd64.tar.gz v2.0.0 && gp up && test "$code" = 0 &&
	contains "v1.0.0 -> v2.0.0" "$out" && grep -q "TOOL = v2.0.0" "$LOCK"
'

test_expect_success TAR 'up re-fetches when dest is gone despite a matching lock' '
	mkdir srcd && echo back >srcd/f.txt && tar czf app.tgz -C srcd . &&
	export SERVE_FILE="$PWD/app.tgz" SERVE_RELEASE="$PWD/rel.json" && serve http &&
	gh_release tool-linux-amd64.tar.gz v1.0.0 &&
	write_sources "[TOOL]
host = $URL
repo = owner/tool
match = linux-amd64
ext = tar.gz
dest = $PWD/inst
" &&
	gp up && test "$code" = 0 && rm -rf inst &&
	gp up && test "$code" = 0 && test "$(cat inst/f.txt)" = back
'

test_expect_success 'up saves a non-archive asset as a file in dest' '
	printf binary >payload.bin &&
	export SERVE_FILE="$PWD/payload.bin" SERVE_RELEASE="$PWD/rel.json" && serve http &&
	gh_release tool-linux-amd64.bin v1.0.0 &&
	write_sources "[TOOL]
host = $URL
repo = owner/tool
match = linux-amd64
ext = bin
dest = $PWD/inst
" &&
	gp up && test "$code" = 0 &&
	test "$(cat inst/tool-linux-amd64.bin)" = binary
'

test_expect_success 'up errors (and lists candidates) on an ambiguous match' '
	export SERVE_RELEASE="$PWD/rel.json" && serve http &&
	printf "%s" "{\"tag_name\":\"v1\",\"assets\":[{\"name\":\"a-linux.tar.gz\",\"browser_download_url\":\"$URL/dl/a\"},{\"name\":\"b-linux.tar.gz\",\"browser_download_url\":\"$URL/dl/b\"}]}" >rel.json &&
	write_sources "[TOOL]
host = $URL
repo = owner/tool
match = linux
ext = tar.gz
dest = $PWD/inst
" &&
	gp up; test "$code" != 0 &&
	contains "ambiguous" "$out" && contains "a-linux.tar.gz" "$out" &&
	! test -e "$LOCK"
'

# GitLab differs in both the JSON shape (assets.links/direct_asset_url, no
# size) and the endpoint (project path URL-encoded, so the slash is %2F);
# apipath records the path gp requested so we can assert the encoding.
test_expect_success TAR 'up resolves a gitlab release (assets.links + %2F path)' '
	mkdir srcd && echo gl >srcd/f.txt && tar czf app.tgz -C srcd . &&
	export SERVE_FILE="$PWD/app.tgz" SERVE_RELEASE="$PWD/rel.json" && serve http &&
	printf "%s" "{\"tag_name\":\"v3.0.0\",\"assets\":{\"links\":[{\"name\":\"tool_linux_amd64.tar.gz\",\"direct_asset_url\":\"$URL/dl/tool.tar.gz\"}]}}" >rel.json &&
	write_sources "[TOOL]
forge = gitlab
host = $URL
repo = group/proj
match = linux_amd64
ext = tar.gz
dest = $PWD/inst
" &&
	gp up && test "$code" = 0 && contains "installing v3.0.0" "$out" &&
	test "$(cat inst/f.txt)" = gl &&
	grep -q "projects/group%2Fproj/releases/permalink/latest" apipath
'

test_expect_success 'up rejects an unknown forge before any fetch' '
	write_sources "[TOOL]
forge = bitbucket
repo = owner/tool
dest = $PWD/inst
" &&
	gp up; test "$code" != 0 && contains "unknown forge" "$out"
'

test_done
