#!/bin/sh
# Real-network throughput benchmark: single stream vs parallel range split,
# same URL. Reports wall time per mode; does NOT pass/fail on timing (the
# network is too noisy for that). Use it to decide whether parallel is
# worth enabling for a given server/link.
#
#   GP_BENCH=1 sh t/bench.sh
#
# parallel helps only when the server caps each connection. Defaults to a
# host known to do so (tele2); override with GP_BENCH_URL. GP_BENCH_CONNS
# sets the connection count, GP_BENCH_REPS the runs per mode (min is kept).

[ "$GP_BENCH" = 1 ] || { echo "set GP_BENCH=1 to run (downloads ~100MB several times)"; exit 0; }

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GP="$ROOT/out/gp"
URL=${GP_BENCH_URL:-http://speedtest.tele2.net/100MB.zip}
CONNS=${GP_BENCH_CONNS:-8}
REPS=${GP_BENCH_REPS:-3}
[ -x "$GP" ] || { echo "build first: (cd $ROOT && go build -o out/)"; exit 1; }
export XDG_CONFIG_HOME=$(mktemp -d)
DST=$(mktemp)
trap 'rm -f "$DST"; rm -rf "$XDG_CONFIG_HOME"' EXIT

# min wall time (ms) of REPS runs of "$@", via gp's own transfer timer.
best_ms() {
	min=
	i=0
	while [ "$i" -lt "$REPS" ]; do
		ms=$("$@" -o "$DST" "$URL" | sed -n 's/.* in \([0-9.]*\)s.*/\1/p' | awk '{printf "%d", $1*1000}')
		[ -z "$min" ] || [ "$ms" -lt "$min" ] && min=$ms
		i=$((i + 1))
	done
	echo "$min"
}

echo "url=$URL conns=$CONNS reps=$REPS (best of $REPS)"
single=$(best_ms env PARALLEL=1 "$GP")
par=$(best_ms env PARALLEL="$CONNS" PARALLEL_MIN=1 "$GP")
echo "single  : ${single}ms"
echo "parallel: ${par}ms"
awk -v s="$single" -v p="$par" 'BEGIN{ if (p>0) printf "speedup : %.2fx\n", s/p }'
