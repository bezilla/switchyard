# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `make demo` and a matching documented curl sequence that walk one request at a
  time: ask, read which provider answered, break exactly that provider, ask
  again and watch a different one answer. The dashboard shows failover in
  aggregate; this shows it for a single call.
- `ROADMAP.md`, covering what comes after v0.1 and why each item is harder than
  it looks.
- Table-driven tests for the chat handler, covering the header-ordering
  guarantee and the no-provider-available path.
- This changelog.

### Changed

- The quickstart opens with `make up` rather than `docker compose up`. The
  latter holds the terminal, and every step after it wants a prompt.
- The identity gate is scoped to `refs/heads/*` and tags, rather than `--all`.
  Refs the repository does not write — remote-tracking refs, and the
  `refs/pull/N/head` copies GitHub keeps forever — were making the gate report
  violations nobody could remove.
- Dependency updates are manual and Dependabot is off, with the reasoning
  written down in `CONTRIBUTING.md`.

### Fixed

- `make help`, the default goal, listed no targets at all. Its grep pattern
  ended in a literal process id where an escaped `$` belonged, so nothing
  matched and the first command a stranger runs printed an empty list.

### Planned

Not started. See [ROADMAP.md](ROADMAP.md) for the full argument.

- **Streaming failover** is the v0.2 headline, and the hard version of what
  v0.1 solves. Once tokens are out there is no transparent retry, so the open
  question is what a caller should be handed instead: buffer and replay, an
  in-band error event, or failing the stream. None of the three is chosen yet.
- **Retries** are a known gap. A request that timed out may still be running
  upstream, so retrying it risks paying for the same completion twice.
- **Idempotency** is a known gap. The key that would make a retry safe needs
  per-provider support the simulated adapters have no honest way to fake.

## [0.1.0] - 2026-08-29

### Added

- A provider gateway that routes inference across simulated providers and
  survives their failures visibly, on a Grafana dashboard, from parsed metrics
  rather than from claims.
- Three simulated providers, each making a different tradeoff: `apex` (fast,
  expensive, fails rarely and completely), `bargain` (cheap, slow, rate limited
  past 14 req/s) and `local` (free, refuses once its six slots are full).
- Failover before the first token, so a provider's refusal is invisible to the
  caller, and a circuit breaker that returns a recovered provider to service on
  a ramp rather than a cliff.
- Health probes that run out of band on every provider, so a breaker can learn
  that an upstream it is avoiding has come back.
- A failure taxonomy that separates rate limits from ill health: a 429 triggers
  failover without counting against a provider's health.
- OpenTelemetry metrics and tracing, a Prometheus scrape endpoint, and Grafana
  dashboards provisioned from checked-in JSON.
- A demo stack over Docker Compose, with make targets that inject faults into
  running providers.
- An end-to-end test that breaks a provider and asserts from parsed Prometheus
  metrics that traffic moved and availability held.
- A pre-push gate enforcing commit identity, attribution and secret scanning,
  with an eleven-case self-test, mirrored server-side in CI.

[Unreleased]: https://github.com/bezilla/switchyard/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/bezilla/switchyard/releases/tag/v0.1.0
