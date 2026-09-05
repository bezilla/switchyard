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

### What CI runs

| job | what it enforces |
|-----|------------------|
| `build` | gofmt, `go vet`, `go build`, `go test -race` |
| `lint` | golangci-lint, pinned version |
| `vuln` | govulncheck, pinned version |
| `identity` | commit identity over all history |
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
