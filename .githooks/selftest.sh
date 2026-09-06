#!/usr/bin/env bash
#
# Proves the pre-push hook rejects what it claims to reject, and -- just as
# important -- accepts what it claims to accept.
#
# A gate nobody has watched fail is a gate nobody knows works, and a gate only
# ever watched to refuse could be one that refuses everything. This builds
# throwaway repositories, commits one kind of history into each, and asserts the
# exit status. Run via `make test-hook`; also run in CI.
#
# Every case captures the status with `|| got=$?` rather than running the gate
# bare and reading `$?`. CI runs its steps under `bash -eo pipefail`, where a
# bare non-zero command kills the step before the assertion is reached -- so the
# other shape reports nothing on exactly the cases it exists to prove. This file
# is run under -e, under plain bash and through its shebang.

set -uo pipefail

HOOK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/pre-push"
[ -x "$HOOK" ] || { echo "selftest: $HOOK is not executable"; exit 1; }

# The server-side copy of the same rules. The hook is range-scoped by whatever
# is being pushed; this one chooses its own scope, so the choice needs testing.
CHECK="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/check-identity.sh"
[ -r "$CHECK" ] || { echo "selftest: $CHECK is not readable"; exit 1; }

GOOD_NAME='Paul Bezilla'
GOOD_EMAIL='bezilla@protonmail.com'
GOOD="${GOOD_NAME} <${GOOD_EMAIL}>"
ZERO='0000000000000000000000000000000000000000'

# The identity a dependency bot actually writes: author is the bot, committer is
# the platform. Neither is canonical, so a commit carrying it must fail the gate
# on any ref that is in scope -- and must not be looked at on any ref that isn't.
BOT_AUTHOR_NAME='dependabot[bot]'
BOT_AUTHOR_EMAIL='49699333+dependabot[bot]@users.noreply.github.com'
BOT_COMMITTER_NAME='GitHub'
BOT_COMMITTER_EMAIL='noreply@github.com'

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok_()  { printf '  ok    %-52s exit %d\n' "$1" "$2"; pass=$((pass + 1)); }
bad_() { printf '  FAIL  %-52s exit %d, wanted %d\n' "$1" "$2" "$3"; fail=$((fail + 1)); }

# run_case <description> <expected exit> -- runs the hook against HEAD of $repo
run_case() {
	local desc="$1" want="$2" sha got=0
	sha="$(git -C "$repo" rev-parse HEAD)"
	printf '%s %s %s %s\n' 'refs/heads/main' "$sha" 'refs/heads/main' "$ZERO" |
		(cd "$repo" && "$HOOK" origin) >/dev/null 2>&1 || got=$?
	[ "$got" -eq "$want" ] && ok_ "$desc" "$got" || bad_ "$desc" "$got" "$want"
}

# run_tag_case <description> <expected exit> <tag> -- pushes a tag ref
run_tag_case() {
	local desc="$1" want="$2" tag="$3" obj got=0
	obj="$(git -C "$repo" rev-parse "refs/tags/$tag")"
	printf '%s %s %s %s\n' "refs/tags/$tag" "$obj" "refs/tags/$tag" "$ZERO" |
		(cd "$repo" && "$HOOK" origin) >/dev/null 2>&1 || got=$?
	[ "$got" -eq "$want" ] && ok_ "$desc" "$got" || bad_ "$desc" "$got" "$want"
}

# run_scope_case <description> <expected exit> -- runs check-identity.sh in $repo
run_scope_case() {
	local desc="$1" want="$2" got=0
	(cd "$repo" && bash "$CHECK") >/dev/null 2>&1 || got=$?
	[ "$got" -eq "$want" ] && ok_ "$desc" "$got" || bad_ "$desc" "$got" "$want"
}

# bot_commit -- adds one commit with bot identity, prints its sha, leaves main
# where it was. The commit survives only through whatever ref the caller sets.
bot_commit() {
	git -C "$repo" checkout -q -b scratch
	echo 'bump' >> "$repo/file.txt"
	git -C "$repo" add file.txt
	GIT_AUTHOR_NAME="$BOT_AUTHOR_NAME" GIT_AUTHOR_EMAIL="$BOT_AUTHOR_EMAIL" \
	GIT_COMMITTER_NAME="$BOT_COMMITTER_NAME" GIT_COMMITTER_EMAIL="$BOT_COMMITTER_EMAIL" \
		git -C "$repo" commit -q -m 'Bump a dependency'
	git -C "$repo" rev-parse HEAD
	git -C "$repo" checkout -q main
	git -C "$repo" branch -q -D scratch
}

# fresh_repo -- a repo with one canonical commit
fresh_repo() {
	repo="$tmp/r$RANDOM$RANDOM"
	git init -q -b main "$repo"
	git -C "$repo" config user.name "$GOOD_NAME"
	git -C "$repo" config user.email "$GOOD_EMAIL"
	echo 'baseline' > "$repo/file.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -q -m 'Add baseline'
}

msg_commit() {
	echo "x $RANDOM" >> "$repo/file.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -q -m "$1"
}

echo 'pre-push gate self-test'

# --- the two enforcement points must carry the same rule ----------------------
# The hook and scripts/check-identity.sh are separate files with different jobs:
# one fails fast over a push range, the other reports counts over all history.
# They share the policy by carrying the same function, and this asserts they
# still do, byte for byte -- drift here would mean the local gate and the CI
# gate quietly disagree about what is allowed.
fn_hook="$(sed -n '/^check_trailers() {/,/^}/p' "$HOOK" | shasum -a 256 | cut -d' ' -f1)"
fn_check="$(sed -n '/^check_trailers() {/,/^}/p' "$CHECK" | shasum -a 256 | cut -d' ' -f1)"
if [ -n "$fn_hook" ] && [ "$fn_hook" = "$fn_check" ]; then
	ok_ 'check_trailers is byte-identical in hook and CI script' 0
else
	bad_ 'check_trailers has DRIFTED between hook and CI script' 1 0
fi

# --- the good case ------------------------------------------------------------
fresh_repo
run_case 'canonical author and committer' 0

# --- bad author, correct committer --------------------------------------------
fresh_repo
echo 'x' >> "$repo/file.txt"; git -C "$repo" add -A
GIT_AUTHOR_NAME='Somebody Else' GIT_AUTHOR_EMAIL='somebody@example.invalid' \
	git -C "$repo" commit -q -m 'Change a file'
run_case 'wrong author' 1

# --- correct author, bad committer --------------------------------------------
fresh_repo
echo 'x' >> "$repo/file.txt"; git -C "$repo" add -A
GIT_COMMITTER_NAME='Some Service' GIT_COMMITTER_EMAIL='noreply@example.invalid' \
	git -C "$repo" commit -q -m 'Change a file'
run_case 'wrong committer' 1

# --- the allowlist, both directions -------------------------------------------
# Reviewed-by is innocuous and is refused anyway. That is the allowlist working:
# the rule is "these three and nothing else", not a list of things to fear.
fresh_repo
msg_commit 'Change a file

Reviewed-by: Someone <someone@example.invalid>'
run_case 'disallowed trailer key' 1

fresh_repo
msg_commit "Change a file

Signed-off-by: ${GOOD}"
run_case 'Signed-off-by, canonical identity' 0

fresh_repo
msg_commit 'Change a file

Signed-off-by: Someone Else <someone@example.invalid>'
run_case 'Signed-off-by, different identity' 1

fresh_repo
msg_commit 'Change a file

Verified: availability held at 99.4% across the failover window.'
run_case 'Verified, free text' 0

fresh_repo
msg_commit 'Change a file

Measured: 3 runs, 0 failures.'
run_case 'Measured, free text' 0

# Tested reads exactly like Verified and Measured and is refused anyway, because
# the allowlist is a list and not a vibe. This is the accepted cost of a tight
# list: the next evidence word needs a one-line change before it can land.
fresh_repo
msg_commit 'Change a file

Tested: every case green on three providers.'
run_case 'unlisted evidence key (Tested)' 1

# git parses only the LAST paragraph as trailers. The same word is prose here
# and a trailer above, which is why this gate uses git's parser and not a
# ^Key: regex.
fresh_repo
msg_commit 'Change a file

Verified: this line is not in the final paragraph.

So it is prose, and this paragraph is what makes it so.'
run_case 'mid-message Key: Value is not a trailer' 0

# --- annotated tags, which nothing checked before -----------------------------
fresh_repo
GIT_COMMITTER_NAME='Some Service' GIT_COMMITTER_EMAIL='noreply@example.invalid' \
	git -C "$repo" tag -a v9.9.9 -m 'Release nine'
run_tag_case 'annotated tag, wrong tagger' 1 'v9.9.9'

fresh_repo
git -C "$repo" tag -a v1.0.0 -m 'Release one'
run_tag_case 'annotated tag, canonical tagger' 0 'v1.0.0'

fresh_repo
git -C "$repo" tag -a v2.0.0 -m 'Release two

Reviewed-by: Someone <someone@example.invalid>'
run_tag_case 'disallowed trailer in a tag annotation' 1 'v2.0.0'

# --- scope of the server-side check -------------------------------------------
# The gate's claim is "every commit I wrote carries my identity". A bot's commit
# on a ref this repository did not author is outside that claim, and treating it
# as a violation makes the gate unsatisfiable: refs/pull/N/head is permanent, so
# there would be no action that clears it.

fresh_repo
run_scope_case 'clean repo passes the scoped check' 0

fresh_repo
sha="$(bot_commit)"
git -C "$repo" update-ref refs/remotes/origin/dependabot/bump "$sha"
run_scope_case 'bot commit on a remote-tracking ref' 0

fresh_repo
sha="$(bot_commit)"
git -C "$repo" update-ref refs/pull/1/head "$sha"
run_scope_case 'bot commit on refs/pull/N/head' 0

# The same commit, one ref namespace over: still a violation.
fresh_repo
sha="$(bot_commit)"
git -C "$repo" update-ref refs/heads/dependabot/bump "$sha"
run_scope_case 'same bot commit on a local branch' 1

# Guards against "fixing" the scope by narrowing it to main. A topic branch is a
# branch this clone can push, so it stays in scope -- for identity and for
# trailers both.
fresh_repo
git -C "$repo" checkout -q -b topic
msg_commit 'Change a file

Reviewed-by: Someone <someone@example.invalid>'
git -C "$repo" checkout -q main
run_scope_case 'disallowed trailer on a non-main local branch' 1

# The CI script must also refuse a tag the hook would refuse -- same rule, and
# tags are in its scope too.
fresh_repo
GIT_COMMITTER_NAME='Some Service' GIT_COMMITTER_EMAIL='noreply@example.invalid' \
	git -C "$repo" tag -a v9.9.9 -m 'Release nine'
run_scope_case 'non-canonical tag tagger, server-side' 1

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
