#!/bin/sh
# git/git-style parity tests for gp. gp must download byte-identical to
# curl, including over the parallel byte-range path (reassembly is the
# thing most likely to corrupt output). Network-gated and hermetic by
# default: the NET tests skip unless GP_NET=1.
#
#   sh t/parity.sh              # NET tests skip
#   GP_NET=1 sh t/parity.sh     # run them
#
# Two pinned release assets, both Accept-Ranges: bytes. The large one is
# fetched with -p to force the parallel split path (off by default); the
# small one covers the plain single-stream path. Override with
# GP_PARITY_SMALL / GP_PARITY_LARGE.

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GP="$ROOT/out/gp"
SMALL=${GP_PARITY_SMALL:-https://github.com/BurntSushi/ripgrep/releases/download/15.1.0/ripgrep-15.1.0-x86_64-unknown-linux-musl.tar.gz}
LARGE=${GP_PARITY_LARGE:-https://github.com/cli/cli/releases/download/v2.62.0/gh_2.62.0_linux_amd64.tar.gz}
H3URL=${GP_PARITY_H3URL:-https://cloudflare-quic.com/}

# --- minimal test harness (git test-lib idiom) -----------------------
test_count=0
test_failed=0
test_prereqs=" "
LOG=$(mktemp)
TRASH=$(mktemp -d)
trap 'rm -rf "$LOG" "$TRASH"' EXIT
cd "$TRASH" || exit 1

# gp must not read a real ~/.config/gp; pin config to an empty dir.
export XDG_CONFIG_HOME="$TRASH/xdg"

say() { printf '%s\n' "$*"; }
test_set_prereq() { test_prereqs="$test_prereqs$1 "; }
have_prereq() { case "$test_prereqs" in *" $1 "*) return 0 ;; esac; return 1; }

test_path_is_file() { [ -f "$1" ] || { echo "not a file: $1"; return 1; }; }
test_path_is_missing() { [ ! -e "$1" ] || { echo "exists: $1"; return 1; }; }
test_cmp() { cmp "$1" "$2"; }

# test_expect_success [PREREQ] 'message' 'script'
test_expect_success() {
	if [ $# -eq 3 ]; then prereq=$1; shift; else prereq=; fi
	msg=$1; body=$2
	test_count=$((test_count + 1))
	if [ -n "$prereq" ] && ! have_prereq "$prereq"; then
		say "ok $test_count - $msg # SKIP (need $prereq)"
		return
	fi
	if eval "$body" >"$LOG" 2>&1; then
		say "ok $test_count - $msg"
	else
		test_failed=$((test_failed + 1))
		say "not ok $test_count - $msg"
		sed 's/^/#   /' "$LOG"
	fi
}

test_done() {
	say "# passed $((test_count - test_failed)) of $test_count"
	[ "$test_failed" = 0 ]
	exit
}

# --- prerequisites ---------------------------------------------------
[ -x "$GP" ] || { echo "build first: (cd $ROOT && go build -o out/)"; exit 1; }
command -v curl >/dev/null 2>&1 && test_set_prereq CURL
[ "$GP_NET" = 1 ] && have_prereq CURL && test_set_prereq NET

# --- tests -----------------------------------------------------------
test_expect_success NET 'reference downloads with curl' '
	curl -fsSL -o ref.small "$SMALL" &&
	curl -fsSL -o ref.large "$LARGE" &&
	test_path_is_file ref.small &&
	test_path_is_file ref.large
'

test_expect_success NET 'small file: gp matches curl (single-stream path)' '
	"$GP" -o gp.small "$SMALL" &&
	test_cmp ref.small gp.small
'

test_expect_success NET 'large file: parallel split (-p 8) matches curl byte-for-byte' '
	"$GP" -p 8 -o gp.large "$LARGE" &&
	test_cmp ref.large gp.large
'

test_expect_success NET 'large file: split result matches a single stream' '
	"$GP" -p 1 -o gp.large1 "$LARGE" &&
	test_cmp gp.large gp.large1
'

test_expect_success NET 'gp reports the downloaded byte count' '
	sz=$(wc -c <ref.large | tr -d " ") &&
	"$GP" "$LARGE" >out.txt &&
	grep -q "bytes=$sz" out.txt
'

test_expect_success NET 'gp --quic negotiates HTTP/3' '
	"$GP" --quic "$H3URL" >h3.txt &&
	grep -q "proto=HTTP/3" h3.txt
'

test_done
