# Contributing

## Step 1, before anything else

```sh
make init
```

This sets `core.hooksPath=.githooks` and the repository-local identity. **It is
required in every clone.** `core.hooksPath` is per-clone configuration: cloning
this repository copies the hook *file* but installs nothing. Until you run this,
the pre-push gate is a file on disk that never executes.

Verify it works:

```sh
make test-hook
```

That builds throwaway repositories with deliberately bad history and asserts the
hook's exit status for each case. A gate nobody has watched fail is a gate nobody
knows works.

---

## Single identity

Every commit in this repository is authored **and** committed by:

```
Paul Bezilla <bezilla@protonmail.com>
```

Both fields, on every commit, with no exceptions. Author and committer are
separate fields in a commit object, and most of the ways history gets rewritten
change one without changing the other.

`make init` sets this locally. It does not touch your global git configuration.

---

## All changes land by direct push

Push to `main`. Through the hook.

```sh
git push origin main
```

The pre-push hook checks, in order:

1. **Identity** — author and committer on every commit being pushed.
2. **Attribution** — no generated-by strings in any commit message, and none in
   the tree at any commit in the range.
3. **Secrets** — gitleaks over history. This fails closed when gitleaks is not
   installed, because a gate that skips when its scanner is missing is not a
   gate.

The same rules run server-side in the `identity` CI job, which is the copy that
runs whether or not anyone remembered step 1.

---

## Never use the merge button

**Do not run `gh pr merge` in any mode. Do not click Merge, Squash or Rebase in
the web UI.**

Every server-side merge mode rewrites identity, and none of them produce the
canonical one:

| mode | what it does to identity |
|------|--------------------------|
| **squash** | stamps GitHub's `noreply` address as **committer** |
| **rebase** | rewrites commits with the merging **account's** identity |
| **merge** | creates a merge commit authored by the merging **account** |

The pre-push hook cannot object to any of these, because the platform performed
the write — no clone was involved and no hook ran. The result is permanent: the
commits are already on the remote, and `refs/pull/N/head` keeps a copy of every
pushed pull request head forever, whether or not the pull request was merged,
closed or deleted.

That permanence is not hypothetical. Two prior repositories had to be deleted and
recreated because identity was wrong at birth and no amount of history rewriting
could remove what `refs/pull/N/head` had already preserved.

If you want review before landing, review the branch locally and then push
`main` directly.

---

## Dependabot pull requests are notifications, not merge requests

[`.github/dependabot.yml`](.github/dependabot.yml) opens pull requests for Go
module and Actions bumps. Read them; never merge them. The rule above has no
exception for automation — a Dependabot pull request merged from the UI stamps a
non-canonical committer exactly the same way any other one does.

Treat each one as a notification that an upstream version moved:

1. **Read the changelog** for the bump. Most are routine; some are not.
2. **If it is worth taking, apply it locally** — `go get`, `go mod tidy`, or edit
   the workflow pin by hand. Do not fetch and cherry-pick the Dependabot branch;
   its commits carry Dependabot's identity, and cherry-picking preserves the
   author field.
3. **Run `make check`,** then push `main` through the hook like any other change.
4. **Close the pull request unmerged.** Dependabot will not reopen it once the
   dependency is already at the target version.

If a bump is not worth taking, close the pull request with a one-line reason.

---

## Forbidden strings

Assistant-attribution terms — the two vendor names and the `co-auth[o]red`
trailer, in any casing — must not appear anywhere in the repository: commit
messages, trailers, code, comments, documentation, configuration, CI, or tags.

The authoritative list is the `FORBIDDEN` pattern in
[`.githooks/pre-push`](.githooks/pre-push), and it is written with
single-character brackets — `c[l]aude`, and so on — so that the checker matches
each term without containing it. In a POSIX extended regex `[x]` matches exactly
`x`, so the pattern is literal while the file's own bytes are not. This document
uses the same trick for the same reason.

If you edit the hook or [`scripts/check-identity.sh`](scripts/check-identity.sh),
keep the brackets. "Simplifying" them away would make both files fail their own
check.

---

## Before you push

```sh
make check     # vet, lint, race tests, identity
make e2e       # the full stack, a real injected failure, assertions from metrics
```

`make check` is what CI runs, minus the end-to-end job. `make e2e` needs Docker
and takes about two minutes.

### What CI runs

| job | what it enforces |
|-----|------------------|
| `build` | gofmt, `go vet`, `go build`, `go test -race` |
| `lint` | golangci-lint, pinned version |
| `vuln` | govulncheck |
| `identity` | canonical identity and no attribution strings, over all history |
| `secrets` | gitleaks over the full history |
| `e2e-failover` | starts the stack, breaks a provider, asserts from parsed metrics |

---

## Code conventions

- Comments explain **why**, not what. If a comment restates the line below it,
  delete one of them.
- Every non-obvious decision that survived an alternative gets a note about the
  alternative — in the code if it is local, in [DESIGN.md](DESIGN.md) if it is
  structural.
- Tests assert on numbers that must have changed, not on the absence of an error.
- American spelling; the linter enforces it.
- Exported identifiers have doc comments; `revive` enforces it.
