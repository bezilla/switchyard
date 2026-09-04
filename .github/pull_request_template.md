Thank you for this — genuinely. You went and did the work, and that deserves a
straight answer rather than silence on an open tab.

**Pull requests are not merged in this repository.** That is a standing policy,
decided before your change existed, and it is not a judgment on the change or on
you.

Here is the reason. This is a personal portfolio repository, and the property it
demonstrates is that every commit in it carries one canonical identity, verified
over all of history by a pre-push hook and again by a CI job that nobody can
forget to install. Every server-side merge mode — Merge, Squash and Rebase alike —
rewrites the author or the committer of what it lands. Using the merge button
once would break the exact property the repository exists to show, permanently
and unfixably without rewriting published history.

There is a second, quieter reason: opening a pull request creates
`refs/pull/N/head` in this repository's ref namespace, and GitHub keeps that ref
forever, whether the pull request is merged, closed, or deleted.

[CONTRIBUTING.md](../CONTRIBUTING.md) has the full argument.

## What actually helps

- **Open an issue.** Bug reports, design disagreements, questions and "this is
  wrong and here is why" are all welcome, and an issue is the right shape for
  every one of them.
- **Put the change in the issue.** A diff, a patch, a branch on your fork, or
  just a clear description — whatever is easiest. If it is right, it gets applied
  locally and pushed through the hook, and the commit message credits you by name
  and links the issue.

Please don't bin the work. Move it to an issue and it will get read.
