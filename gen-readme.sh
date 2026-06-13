#!/bin/bash
# Regenerate the Usage/Configuration tail of readme.md from gp's live --help
# so docs can't drift from the flags. Everything from the marker to EOF is
# owned by this script; the hand-written intro above it is preserved.
# Wired via .pre-commit-config.yaml; run by hand with ./gen-readme.sh.
set -e

marker='<!-- gp-help:begin (generated; edit flags or gen-readme.sh, not below) -->'
help=$(go run . --help 2>&1)
fence='```'

# Keep everything above the marker, then re-emit the generated block. A temp
# file avoids truncating readme.md while awk is still reading it.
{
	awk -v m="$marker" '$0 == m { exit } { print }' readme.md
	cat <<EOF
$marker

## Usage

$fence
$help
$fence

## Configuration

Keys resolve in order: real env > .env > ini > default. See gpconfig.ini for
the annotated sample (XDG path: \$XDG_CONFIG_HOME/gp/gpconfig.ini).
EOF
} > readme.md.tmp && mv readme.md.tmp readme.md
