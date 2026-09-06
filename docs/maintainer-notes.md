# Maintainer notes

Operational notes for the person who pushes here. Nothing in this file is
needed to build, run, test or read the project; see
[CONTRIBUTING.md](../CONTRIBUTING.md) for that.

---

## Pushing when the required checks have not run yet

`main` is protected, and the protection requires all six CI checks to pass on
the commit being pushed. Both workflows trigger on `push` to `main`, so a new
commit has no passing checks at the moment of the push and the push is refused:

```
remote: error: GH006: Protected branch update failed for refs/heads/main.
remote: error: Required status check "build · vet · test" is expected.
```

That is not a misconfiguration to relax. The checks are the point; the ordering
is the problem.

The procedure is to lift the narrowest thing that unblocks the push — the
bypass GitHub grants repository admins by default, which is a single boolean on
the protection object — push through the hook exactly as normal, and put it
straight back. Required checks, linear history, and the force-push and deletion
bans all stay on the whole time. Removing the protection object entirely would
also unblock the push, and is the thing not to do: between the removal and the
restore the branch is unprotected, and every setting then has to be rebuilt by
hand from a snapshot.

Two steps carry the whole safety of this, and neither is optional:

1. **Snapshot the entire protection object to a file first.** Restoring from
   memory is how protection quietly comes back weaker than it went out.
2. **Diff it against that snapshot afterwards**, in the same shell, whether or
   not the push succeeded. An unverified restore is indistinguishable from a
   forgotten one right up until the day it matters.

The object is read and written through `gh api` on
`repos/OWNER/REPO/branches/main/protection`, and compared with
`diff <(jq -S . before.json) <(jq -S . after.json)`.

---

## Why the merge button is not an alternative

It is the obvious question, and the answer is in
[CONTRIBUTING.md](../CONTRIBUTING.md): every server-side merge mode rewrites at
least one identity field, and none of them produce the canonical one. Squash
stamps the platform's `noreply` address as committer; Rebase and Merge stamp the
merging account's identity. The pre-push hook cannot object, because the
platform performed the write and no clone was involved.

The result is permanent. The commits are already on the remote, and
`refs/pull/N/head` keeps a copy of every pushed pull request head forever,
whether or not the pull request was merged, closed or deleted. Two earlier
repositories had to be deleted and recreated over exactly this.

---

## Editing the identity gate

`.githooks/pre-push` and `scripts/check-identity.sh` enforce the same rule from
two places, and they carry the same `check_trailers()` function to do it —
**byte for byte**. `.githooks/selftest.sh` hashes the function out of both files
and fails if they differ, so the local gate and the CI gate cannot quietly
disagree about what is allowed.

If you change the allowlist, change it in both files. The test will tell you if
you forget, which is the point of it existing.

They are separate files rather than one sourced library because they do
different jobs: the hook fails fast on the first problem in a push range, and
`check-identity.sh` reports counts for every scan over all history. A shared
library would remove the duplication and add a path dependency between
`.githooks/` and `scripts/`; the hashed-function test buys the same guarantee
without the coupling. If a third caller ever appears, that trade flips.

The gate previously matched a list of vendor terms written with single-character
brackets, so each file could scan for a term without containing it. That scan is
gone — it matched nothing across 207 commits of full history in six
repositories — and the brackets went with it.
