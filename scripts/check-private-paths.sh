#!/bin/sh
# Fails when loupe-private's docs/internals cite a Go file that no longer
# exists in this tree, so a change that moves, renames or deletes one also
# updates those docs.
#
# It reads the docs at LOUPE_PRIVATE_REF (default origin/main, as last
# fetched) in LOUPE_PRIVATE (default ~/dev/loupe-private), not the working
# copy, which may be on any branch. While a private docs PR is open, point
# LOUPE_PRIVATE_REF at its branch. Without the private repo (CI), it skips.
set -eu

priv=${LOUPE_PRIVATE:-$HOME/dev/loupe-private}
ref=${LOUPE_PRIVATE_REF:-origin/main}

if ! git -C "$priv" rev-parse --verify -q "$ref^{commit}" >/dev/null 2>&1; then
	echo "check-private-paths: no $ref in $priv, skipped"
	exit 0
fi

missing=$(
	git -C "$priv" ls-tree --name-only "$ref" docs/internals/ |
		grep '\.md$' |
		while read -r f; do git -C "$priv" show "$ref:$f"; done |
		grep -oE '(internal|cmd)/[A-Za-z0-9_/.-]+\.go' |
		sort -u |
		while read -r p; do [ -e "$p" ] || echo "$p"; done
)

if [ -n "$missing" ]; then
	echo "loupe-private $ref docs/internals cite files that no longer exist here:"
	echo "$missing" | sed 's/^/  /'
	echo "Update those docs in loupe-private, then set LOUPE_PRIVATE_REF to that branch until it merges."
	exit 1
fi
echo "check-private-paths: every path cited in loupe-private $ref docs/internals exists"
