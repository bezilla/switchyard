# Contributing

This is a solo repository. Changes land by direct push to `main`, with CI
enforced on every push, and pull requests are not merged here. That is a
standing policy rather than a judgment on any particular change.

The mechanical reason, so it does not have to be guessed at: every commit on
`main` has to carry one canonical identity, and every server-side merge mode
rewrites at least one identity field. Squash stamps the platform's `noreply`
address as the committer; Rebase and Merge stamp the merging account's identity.
None of the three produce the required identity, the hook cannot object because
the platform performed the write, and it is not fixable afterwards without
rewriting published history. So the merge button is not an option here, and
"merge your own pull requests" would reintroduce exactly the problem the rule
exists to prevent.

**Issues are open and welcome.** Bug reports, design disagreements and questions
all belong there. A patch described in an issue — a diff, a branch on a fork, or
a clear description — gets read, and if it is right it gets applied and pushed
with credit in the commit message.

## Setting up a clone

```sh
make init       # sets core.hooksPath and the repository-local identity
make test-hook  # proves the pre-push gate rejects bad history
```

`core.hooksPath` is per-clone configuration: cloning copies the hook *file* and
installs nothing, so `make init` is required in every clone. The same rules run
server-side in the `identity` CI job, which is the copy nobody can forget to
install. The hook also runs `gitleaks` over history and fails closed when it is
not installed, because a secrets gate that skips when its scanner is missing is
not a gate.

## Before you push

```sh
make check     # vet, lint, race tests, identity
make e2e       # the full stack, a real injected failure, assertions from metrics
```

`make check` is what CI runs, minus the end-to-end job. `make e2e` needs Docker
and takes about two minutes.

### Commit trailers are allowlisted

Only three trailer keys may appear on a commit, or in an annotated tag's body.
Every other key is refused:

| trailer | rule |
|---------|------|
| `Signed-off-by` | must be exactly `Paul Bezilla <bezilla@protonmail.com>` |
| `Verified` | free text |
| `Measured` | free text |

This replaced a denylist that grepped trailers for `generated|assisted|
on-behalf-of`. A denylist catches the words somebody thought of and is stale the
day a tool ships using a fourth; an allowlist refuses an unlisted key whether or
not the gate has heard of what wrote it. Both `.githooks/pre-push` and
`scripts/check-identity.sh` carry the same `check_trailers()` function byte for
byte, and `make test-hook` fails if they ever differ.

#### The trailer rule has one sharp edge

Whether a `Key: Value` line is a trailer depends on **which paragraph it lands
in**. git parses only the last paragraph, and only when the whole paragraph
parses as trailers:

```
Route around a dead provider         Route around a dead provider

Verified: availability held.         Verified: availability held.

And a closing paragraph.             ← nothing after it
```

The left-hand message ends in prose, so `Verified:` there is ordinary text the
gate never looks at. The right-hand one ends with that line, so it **is** a
trailer and its key must be allowlisted. Same words, two outcomes, decided by
what comes after.

That is git's own definition, read with `git interpret-trailers --parse`, and it
is the definition the tools that stamp provenance use. A `^Key:` regex would be
simpler and would reject this repository's own prose — six lines here are
`Key: Value` shaped and are not trailers: `hold:`, `load:`, `claim:`, `step:`,
`one:` and `Verified:`.

If a push is refused for a trailer you thought was prose, check whether it ended
up last. A new evidence word — `Tested:`, `Confirmed:` — needs adding to the
allowlist in both files before it can land there. That is the accepted cost of a
tight list.

### What the allowlist does not catch, on purpose

Two things pass this gate that an earlier version of it would have stopped. Both
are the deliberate reduction, not an oversight.

**A vendor or tool name in the body of a message.** The allowlist reads the
trailer block and nothing else, so such a name written in a paragraph of prose is
ordinary text and is accepted. Attribution is stamped as a trailer, and an
unlisted key is refused whether or not the gate has heard of the tool that wrote
it — a stronger guarantee than a name list can give, because it does not need
updating when a new tool ships. Matching words in prose is a different job, and
the denylist that did it matched nothing across the full history of every
repository in this family.

**Anything in the working tree.** Nothing greps the checkout for vendor names.
Hand-written hooks under `.git/hooks/` once did, and `core.hooksPath` makes git
ignore that directory entirely, so any that survive there are inert. They have
not been restored and should not be: it is the same scan with the same zero
matches, and it walked build artefacts, so a full validation run could leave a
clean tree unpushable.

The same trade is taken in every repository that shares this gate. Consistency
across them is the property worth keeping — a one-repository exception would be
the defect, not the fix.

**History was not rewritten when this changed.** No force push, no retag, nothing
dropped; only the rule applied to new pushes is different.

### What CI runs

| job | what it enforces |
|-----|------------------|
| `build` | gofmt, `go vet`, `go build`, `go test -race` |
| `lint` | golangci-lint, pinned version |
| `vuln` | govulncheck, pinned version |
| `identity` | commit identity, the trailer allowlist and tag taggers, over all history and both tags |
| `secrets` | gitleaks over the full history |
| `e2e-failover` | starts the stack, breaks a provider, asserts from parsed metrics |

## Dependencies are pinned, and updated by hand

Every third-party reference is pinned to an immutable identifier: GitHub Actions
by commit SHA, container images by digest, tools by version. A tag is a mutable
pointer in somebody else's repository, and "the build changed and nothing in git
did" is the class of problem pinning exists to prevent.

Keeping those pins current is [Renovate](renovate.json5)'s job. It is configured
to write one dependency-dashboard issue and nothing else — no branches, no pull
requests — because a bot that opens a pull request creates `refs/pull/N/head`,
which GitHub keeps permanently whether the pull request is merged, closed or
deleted. Dependabot is off for the same reason and there is no `dependabot.yml`.

So an update is hand-work either way:

```sh
go get example.com/mod@vX.Y.Z && go mod tidy
make check
git push origin main
```

`govulncheck` runs in CI on every push, so a dependency with a known advisory on
a reachable call path fails the build.

## Code conventions

- Comments explain **why**, not what. If a comment restates the line below it,
  delete one of them.
- Every non-obvious decision that survived an alternative gets a note about the
  alternative — in the code if it is local, in [DESIGN.md](DESIGN.md) if it is
  structural.
- Tests assert on numbers that must have changed, not on the absence of an error.
- American spelling; the linter enforces it.
- Exported identifiers have doc comments; `revive` enforces it.
