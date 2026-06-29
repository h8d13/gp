#!/bin/sh
# Config-validation suite for sources.ini, exercised through `gp up check`
# (parse + validate, no network). The shipped sample is checked too, so a broken
# sample is caught here rather than by a user who copied it. Hermetic: each case
# writes its own sources.ini under a fresh XDG dir and never fetches.
#
#   sh t/srcs-e2e.sh

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GP="$ROOT/out/gp"
go build -C "$ROOT" -o out/ . || { echo "build failed"; exit 1; }

TRASH=$(mktemp -d)
trap 'rm -rf "$TRASH"' EXIT
cd "$TRASH" || exit 1

count=0
failed=0

# run `gp up check` against the sources.ini content in $1, under a fresh XDG dir
# so no case leaks into the next. Captures combined output in $out, status $code.
check() {
	rm -rf xdg && mkdir -p xdg/gp
	printf '%s' "$1" >xdg/gp/sources.ini
	out=$(XDG_CONFIG_HOME="$PWD/xdg" "$GP" up check 2>&1)
	code=$?
}
# ok NAME COND: COND is a shell expression asserted over $out/$code.
ok() {
	count=$((count + 1))
	if eval "$2"; then
		echo "ok $count - $1"
	else
		echo "not ok $count - $1"
		printf '%s\n' "$out" | sed 's/^/  # /'
		failed=$((failed + 1))
	fi
}
has() { case "$out" in *"$1"*) return 0 ;; esac; return 1; }

# The shipped sample must validate, every section.
rm -rf xdg && mkdir -p xdg/gp && cp "$ROOT/sources.ini" xdg/gp/sources.ini
out=$(XDG_CONFIG_HOME="$PWD/xdg" "$GP" up check 2>&1)
code=$?
ok 'shipped sources.ini validates' 'test "$code" = 0 && has "sources OK"'

check '[T]
url = http://example.invalid/f
dest = /opt/t
'
ok 'a minimal bare-url source validates' 'test "$code" = 0'

check '[T]
url = http://example.invalid/f
'
ok 'missing dest is rejected' 'test "$code" != 0 && has "dest is required"'

check '[T]
url = http://example.invalid/f.bin
as = a/b
dest = /opt/t
'
ok 'as= with a slash is rejected' 'test "$code" != 0 && has "bare filename"'

check '[T]
url = http://example.invalid/f
sha256 = deadbeef
dest = /opt/t
'
ok 'malformed sha256 is rejected' 'test "$code" != 0 && has "64 hex chars"'

check '[T]
url = http://example.invalid/f
sha256 = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
sha256-url = http://example.invalid/sums
dest = /opt/t
'
ok 'sha256 and sha256-url together are rejected' 'test "$code" != 0 && has "not both"'

check '[T]
forge = bitbucket
repo = a/b
dest = /opt/t
'
ok 'unknown forge is rejected' 'test "$code" != 0 && has "unknown forge"'

check '[T]
index = http://example.invalid/d/
dest = /opt/t
'
ok 'index without match/ext is rejected' 'test "$code" != 0 && has "match or ext"'

check '[T]
dest = /opt/t
'
ok 'a source with no url/index/repo is rejected' 'test "$code" != 0 && has "url, index, or repo"'

# --- gp rm: forget a source's lock entry, delete nothing (no network) ---
rm -rf xdg && mkdir -p xdg/gp
printf '[T]\nurl = http://example.invalid/f\ndest = ~/opt/t\n' >xdg/gp/sources.ini
printf '[installed]\nT = v9\n' >xdg/gp/sources.lock
export XDG_CONFIG_HOME="$PWD/xdg"

out=$("$GP" rm T 2>&1)
code=$?
ok 'rm drops the lock entry' 'test "$code" = 0 && ! grep -q "T = v9" xdg/gp/sources.lock'
ok 'rm prints the dest with a leading ~ (not expanded)' 'has "~/opt/t" && ! has "$HOME/opt/t"'

out=$("$GP" rm NOPE 2>&1)
code=$?
ok 'rm errors on an unknown source' 'test "$code" != 0 && has "no source named"'

out=$("$GP" rm 2>&1)
code=$?
ok 'rm with no name errors' 'test "$code" != 0'

out=$("$GP" up rm T 2>&1)
code=$?
ok 'up rm is rejected (rm is its own verb)' 'test "$code" != 0 && has "unknown subcommand"'

echo "# passed $((count - failed)) of $count"
[ "$failed" = 0 ]
