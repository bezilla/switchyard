#!/usr/bin/env bash
#
# Verify that every commit in history carries the one canonical identity, that
# every annotated tag is tagged by that identity, and that no commit or tag
# carries a trailer outside the allowlist.
#
# This is the same rule the pre-push hook enforces, moved to where a clone
# cannot skip it. core.hooksPath is per-clone configuration: cloning this
# repository does not install the hook, and `make init` is a step somebody can
# forget. CI is the copy that runs whether or not anyone remembered.
#
# The trailer rule is an allowlist: Signed-off-by carrying exactly the canonical
# identity, Verified and Measured carrying free text, everything else refused.
# It replaced a denylist that matched trailers against a fixed set of words --
# which caught only the words somebody had thought of, and was stale the day an
# unanticipated one appeared.
#
# check_trailers below is BYTE-IDENTICAL to the copy in .githooks/pre-push, and
# .githooks/selftest.sh asserts that, so the local gate and this one cannot
# drift apart. They are separate files on purpose: the hook fails fast on the
# first problem in a push range, this reports counts for every scan over all
# history. Same rule, two jobs.
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
# assertion, the trailer allowlist and the walk over every commit in range are
# exactly what they were. Narrowing which refs are examined is not the same as
# narrowing what is checked on them. Do not narrow this further to main alone:
# a local topic branch is a branch this clone can push, so it is in scope.

set -uo pipefail

CANONICAL='Paul Bezilla <bezilla@protonmail.com>'
ALLOWED_TRAILERS='Signed-off-by, Verified, Measured'

# Every walk below uses this. One definition so the four scans cannot drift.
SCOPE=(--branches --tags)

status=0
note() { printf '%s\n' "$1"; }
bad() { printf 'identity: %s\n' "$1" >&2; status=1; }

check_trailers() {
	local what="$1" text="$2" trailers line key value
	trailers="$(printf '%s\n' "$text" | git interpret-trailers --parse 2>/dev/null || true)"
	[ -z "$trailers" ] && return 0

	while IFS= read -r line; do
		[ -z "$line" ] && continue
		key="${line%%:*}"
		value="${line#*:}"
		value="${value# }"
		case "$key" in
			'Signed-off-by')
				if [ "$value" != "$CANONICAL" ]; then
					printf 'trailer: %s carries "Signed-off-by: %s", expected "%s"\n' \
						"$what" "$value" "$CANONICAL" >&2
					return 1
				fi
				;;
			'Verified'|'Measured')
				: # free text, by design
				;;
			*)
				printf 'trailer: %s carries disallowed trailer "%s" (allowed: %s)\n' \
					"$what" "$key" "$ALLOWED_TRAILERS" >&2
				return 1
				;;
		esac
	done <<< "$trailers"
	return 0
}

# --- 1. one identity, as author and as committer, on every commit -------------
identities="$(git log "${SCOPE[@]}" --format='%an <%ae>%n%cn <%ce>' | sort -u)"
count="$(printf '%s\n' "$identities" | grep -c .)"

note "distinct author/committer identities in history: ${count}"
printf '%s\n' "$identities" | sed 's/^/  /'

if [ "$count" -ne 1 ] || [ "$identities" != "$CANONICAL" ]; then
	bad "expected exactly one identity, '${CANONICAL}'"
fi

# --- 2. no trailer outside the allowlist, on any commit ----------------------
trailer_hits=0
while read -r sha; do
	[ -z "$sha" ] && continue
	check_trailers "commit ${sha:0:12}" "$(git show -s --format='%B' "$sha")" ||
		trailer_hits=$((trailer_hits + 1))
done < <(git rev-list "${SCOPE[@]}")

note "commits carrying a trailer outside the allowlist: ${trailer_hits}"
if [ "$trailer_hits" -ne 0 ]; then
	bad "commit trailers outside the allowlist (${ALLOWED_TRAILERS})"
fi

# --- 3. annotated tags: tagger identity and annotation body -------------------
# Nothing checked either before. A tag carries an identity field of its own and
# a message of its own, so it was a place to put what the commit gate refused.
tag_count=0
tagger_hits=0
tag_trailer_hits=0
while read -r ref obj; do
	[ -z "${obj:-}" ] && continue
	[ "$(git cat-file -t "$obj" 2>/dev/null || true)" = 'tag' ] || continue
	tag_count=$((tag_count + 1))
	raw="$(git cat-file tag "$obj")"
	tagger="$(printf '%s\n' "$raw" | sed -n 's/^tagger \(.*\) [0-9][0-9]* [-+][0-9][0-9][0-9][0-9]$/\1/p' | head -1)"
	if [ "$tagger" != "$CANONICAL" ]; then
		printf 'identity: tag %s tagger is %s\n' "${ref#refs/tags/}" "$tagger" >&2
		tagger_hits=$((tagger_hits + 1))
	fi
	check_trailers "tag ${ref#refs/tags/}" "$(printf '%s\n' "$raw" | sed '1,/^$/d')" ||
		tag_trailer_hits=$((tag_trailer_hits + 1))
done < <(git for-each-ref --format='%(refname) %(objectname)' refs/tags)

note "annotated tags checked: ${tag_count}"
note "tags whose tagger is not the canonical identity: ${tagger_hits}"
note "tags carrying a trailer outside the allowlist: ${tag_trailer_hits}"
[ "$tagger_hits" -eq 0 ] || bad 'annotated tags carry a non-canonical tagger'
[ "$tag_trailer_hits" -eq 0 ] || bad "tag annotations carry trailers outside the allowlist (${ALLOWED_TRAILERS})"

exit "$status"
