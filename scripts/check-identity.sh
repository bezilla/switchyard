#!/usr/bin/env bash
#
# Verify that every commit in history carries the one canonical identity and
# carries no attribution string, in messages or in the tree.
#
# This is the same rule the pre-push hook enforces, moved to where a clone
# cannot skip it. core.hooksPath is per-clone configuration: cloning this
# repository does not install the hook, and `make init` is a step somebody can
# forget. CI is the copy that runs whether or not anyone remembered.
#
# The forbidden terms are written with single-character brackets so that this
# file matches them without containing them: in a POSIX extended regex [x]
# matches exactly x. Do not "simplify" the brackets away.

set -uo pipefail

CANONICAL='Paul Bezilla <bezilla@protonmail.com>'
FORBIDDEN='c[l]aude|anthrop[i]c|co-auth[o]red'

status=0
note() { printf '%s\n' "$1"; }
bad() { printf 'identity: %s\n' "$1" >&2; status=1; }

# --- 1. one identity, as author and as committer, on every commit -------------
identities="$(git log --all --format='%an <%ae>%n%cn <%ce>' | sort -u)"
count="$(printf '%s\n' "$identities" | grep -c .)"

note "distinct author/committer identities in history: ${count}"
printf '%s\n' "$identities" | sed 's/^/  /'

if [ "$count" -ne 1 ] || [ "$identities" != "$CANONICAL" ]; then
	bad "expected exactly one identity, '${CANONICAL}'"
fi

# --- 2. no attribution strings in any commit message --------------------------
msg_hits="$(git log --all --format='%H %B' | grep -icE "$FORBIDDEN" || true)"
note "commit-message lines matching a forbidden attribution term: ${msg_hits}"
if [ "$msg_hits" -ne 0 ]; then
	git log --all --format='%H%n%B' | grep -inE "$FORBIDDEN" | sed 's/^/  /' >&2
	bad 'commit messages contain forbidden attribution terms'
fi

# --- 3. no attribution strings in any tree, at any commit ---------------------
# Every commit, not just the tip: a term that arrived in one commit and was
# deleted in the next is still in the published history, and a clone can still
# read it.
tree_hits=0
while read -r sha; do
	[ -z "$sha" ] && continue
	hits="$(git grep -ilE "$FORBIDDEN" "$sha" -- . 2>/dev/null || true)"
	if [ -n "$hits" ]; then
		tree_hits=$((tree_hits + 1))
		printf '%s\n' "$hits" | sed 's/^/  /' >&2
	fi
done < <(git rev-list --all)

note "commits whose tree contains a forbidden attribution term: ${tree_hits}"
if [ "$tree_hits" -ne 0 ]; then
	bad 'repository contents contain forbidden attribution terms'
fi

# --- 4. no signed-off or generated-by trailers of any kind --------------------
trailers="$(git log --all --format='%(trailers:only)' | grep -icE 'generated|assisted|on-behalf-of' || true)"
note "commit trailers claiming generation or assistance: ${trailers}"
if [ "$trailers" -ne 0 ]; then
	bad 'commit trailers claim generation or assistance'
fi

exit "$status"
