#!/bin/sh
# simcap-guard.sh SRC REF: refuse to replace .build/simcap with a build of SRC's
# HEAD when that drops changes the current build carries.
#
# .build/simcap.ref holds the commit the current build came from. Replacing it
# is safe when that commit is an ancestor of HEAD, or when it was squash-merged:
# every file it changed since the two diverged matches HEAD. Anything else,
# including a build with no record, may be an unmerged branch a running server
# depends on, so the guard stops and FORCE=1 overrides it.
set -u
src=$1 ref=$2
[ -e .build/simcap ] || exit 0
[ "${FORCE:-}" = 1 ] && exit 0

if [ ! -s .build/simcap.ref ]; then
	echo "simcap: .build/simcap has no .build/simcap.ref, so its commit is unknown;"
	echo "simcap: it may be a build of an unmerged branch. FORCE=1 replaces it."
	exit 1
fi
old=$(cat .build/simcap.ref)
new=$(git -C "$src" rev-parse HEAD)
[ "$old" = "$new" ] && exit 0

git -C "$src" fetch -q --unshallow origin "$ref" 2>/dev/null
git -C "$src" cat-file -e "$old^{commit}" 2>/dev/null || git -C "$src" fetch -q origin "$old" 2>/dev/null

git -C "$src" merge-base --is-ancestor "$old" "$new" 2>/dev/null && exit 0
if base=$(git -C "$src" merge-base "$old" "$new" 2>/dev/null); then
	files=$(git -C "$src" diff --name-only "$base" "$old")
	# shellcheck disable=SC2086 # one path per word
	[ -n "$files" ] && git -C "$src" diff --quiet "$old" "$new" -- $files && exit 0
fi

echo "simcap: .build/simcap was built from $old, which $ref ($new) does not contain;"
echo "simcap: replacing it would drop those changes. FORCE=1 replaces it."
exit 1
