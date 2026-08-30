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
#
# SCOPE: refs/heads/* and refs/tags/*, which is to say the branches and tags
# this repository publishes. Deliberately NOT --all.
#
# The rule is "every commit I wrote carries my identity", not "every object
# that has ever landed in this repository's ref namespace". Those are different
# claims, and only the first one is mine to keep. Anything a bot pushes lands
# under refs/remotes/* in a clone that fetched it, and GitHub keeps a permanent
# copy of every pull request head under refs/pull/N/head whether or not the pull
# request was merged. Neither is reachable from --branches --tags, and neither
# was written here. Scanning them makes the gate report a violation for a commit
# nobody in this repository authored and nobody can remove, which is a gate that
# cannot be satisfied -- and a gate that cannot be satisfied gets switched off.
#
# What this does NOT relax: for every ref that is in scope, the identity
# assertion, the attribution scans and the walk over every commit in range are
# exactly what they were. Narrowing which refs are examined is not the same as
# narrowing what is checked on them. Do not narrow this further to main alone:
# a local topic branch is a branch this clone can push, so it is in scope.

set -uo pipefail

CANONICAL='Paul Bezilla <bezilla@protonmail.com>'
FORBIDDEN='c[l]aude|anthrop[i]c|co-auth[o]red'

# Every walk below uses this. One definition so the four scans cannot drift.
SCOPE=(--branches --tags)

status=0
note() { printf '%s\n' "$1"; }
bad() { printf 'identity: %s\n' "$1" >&2; status=1; }

# --- 1. one identity, as author and as committer, on every commit -------------
identities="$(git log "${SCOPE[@]}" --format='%an <%ae>%n%cn <%ce>' | sort -u)"
count="$(printf '%s\n' "$identities" | grep -c .)"

note "distinct author/committer identities in history: ${count}"
printf '%s\n' "$identities" | sed 's/^/  /'

if [ "$count" -ne 1 ] || [ "$identities" != "$CANONICAL" ]; then
	bad "expected exactly one identity, '${CANONICAL}'"
fi

# --- 2. no attribution strings in any commit message --------------------------
msg_hits="$(git log "${SCOPE[@]}" --format='%H %B' | grep -icE "$FORBIDDEN" || true)"
note "commit-message lines matching a forbidden attribution term: ${msg_hits}"
if [ "$msg_hits" -ne 0 ]; then
	git log "${SCOPE[@]}" --format='%H%n%B' | grep -inE "$FORBIDDEN" | sed 's/^/  /' >&2
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
done < <(git rev-list "${SCOPE[@]}")

note "commits whose tree contains a forbidden attribution term: ${tree_hits}"
if [ "$tree_hits" -ne 0 ]; then
	bad 'repository contents contain forbidden attribution terms'
fi

# --- 4. no signed-off or generated-by trailers of any kind --------------------
trailers="$(git log "${SCOPE[@]}" --format='%(trailers:only)' | grep -icE 'generated|assisted|on-behalf-of' || true)"
note "commit trailers claiming generation or assistance: ${trailers}"
if [ "$trailers" -ne 0 ]; then
	bad 'commit trailers claim generation or assistance'
fi

exit "$status"
