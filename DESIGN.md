# Design

Why Switchyard is built the way it is, and what it is not. Every section below
that names a decision also names what was rejected, because the rejected option
is usually the more obvious one.

---

## The thesis

The problems people describe as new when they put an LLM in production are the
problems a CDN team solved a long time ago. "Which upstream should serve this
request", "this upstream is degraded, use another one", "we are being rate
limited", "this is costing more than we budgeted", "is it the provider or is it
us" — these are traffic steering, origin failover, quota management, cost
control and observability. The vocabulary is new. The problems are not.

Switchyard exists to make that argument concretely rather than in an essay. It
is a gateway you can break and watch recover, and everything it does has a
direct ancestor in edge infrastructure.

---

## Failover happens before the first token

**Decision.** The `Provider` interface splits starting a stream from reading it.
`Start` either returns a live `Stream` or fails; it never partially succeeds.
Every simulated failure mode is surfaced from `Start`.

**Why.** This is the boundary that makes transparent failover possible. Until the
first byte reaches the client, the gateway can silently try somebody else. After
that, it cannot: HTTP status and headers are already sent, and tokens the caller
has seen cannot be unseen. Concentrating refusals at `Start` maximizes the region
where rerouting is invisible.

**Rejected: buffer the whole completion, then decide.** Buffering would make the
entire request reroutable — a provider that dies at token 200 could be retried
elsewhere and the caller would never know. It also destroys streaming, which is
the property that makes an LLM feel responsive at all, and it moves time to first
token from ~90 ms to however long the full completion takes. The narrower
guarantee is worth more than the wider one.

**Rejected: retry mid-stream and stitch.** Restart the failed request on another
provider and continue emitting. The second provider has no idea what the first
one said, so the output would contradict itself mid-sentence. Some gateways do
this. It produces a worse failure than the one it hides.

---

## Failures are classified, not counted

**Decision.** `FailureKind` distinguishes `unavailable`, `timeout`,
`rate_limited` and `capacity`. All four trigger failover. Only `unavailable` and
`timeout` count against a provider's health.

**Why.** A provider returning 429 is working correctly and telling us something
true about our request rate. If that counted against health, the breaker would
open, the breaker would then keep traffic away, the 429s would stop because
nobody was asking, and the only evidence that would ever close the breaker again
is traffic the breaker is refusing to send. The provider would be shunned
permanently for the crime of enforcing its own documented limit.

Capacity refusals from the local provider are the same shape: a full box saying
"not now" is not a broken box.

**Rejected: one error counter for everything.** Simpler, and wrong in the
specific case the project is about. A gateway that cannot tell "you are asking
too fast" from "I am broken" makes exactly the wrong decision under load.

**Rejected: separate breakers per failure kind.** Correct in principle and hard
to reason about on a dashboard, which is where these decisions get debugged. One
breaker with a classification rule in front of it was the better trade.

---

## Recovery is gradual

**Decision.** The breaker has three states, and the third is a ramp. On leaving
`open` it admits 5% of traffic, multiplying by 1.6 every 900 ms of uninterrupted
success until it reaches 100% and closes. Failures during the ramp send it back
to `open` with a longer cooldown.

**Why.** The textbook half-open state admits a single probe and, if that probe
succeeds, closes fully. That transition is a stampede generator. A provider that
has been down for a minute comes back to cold caches, cold connection pools and
an empty scheduler, and the first thing it meets is one hundred percent of the
traffic it was failing under. It falls over, the breaker reopens, and the system
oscillates — the second outage is caused by the recovery from the first.

One probe succeeding is evidence that the provider can serve one request. It is
not evidence that it can serve the offered load. The ramp is how you find out.

**Rejected: single-probe half-open.** The standard. Rejected for the reason
above, which is the most interesting thing this project has to say about
breakers.

**Rejected: fixed-slope linear ramp.** Predictable, and too slow at the start or
too fast at the end depending on where you set the slope. Geometric growth
spends real time at low volume, which is where the risk is, and then gets out of
the way.

**Rejected: reopen on any single failure during the ramp.** Too brittle. Apex has
a 0.2% baseline error rate even when healthy, so a provider with a normal error
rate could never finish recovering. The ramp is judged by the same failure ratio
as the closed state.

---

## Health probes exist because breakers are circular

**Decision.** A background prober calls every provider on a one-second interval
and feeds the result into that provider's breaker, alongside real request
outcomes. Successive probe successes while open cut the cooldown short.

**Why.** A breaker that learns only from traffic has a circularity problem: once
it opens, it stops sending traffic, and traffic was its only source of
information. It cannot notice that an idle provider died, and — worse for a
failover system — it cannot notice that a provider it is avoiding has recovered.
Time-based cooldowns paper over this by guessing. The probe is the out-of-band
signal that turns a guess into an observation.

Cutting the cooldown short on probe success matters for the demo and for real
operations: after repeated trips the backoff reaches tens of seconds, and that
backoff was sized by an outage that has now ended. Making an operator wait it out
after the provider has demonstrably recovered is punishing them for the
provider's earlier behavior.

**Rejected: probes as a separate health signal from the breaker.** Two sources of
truth about whether a provider is usable, which then disagree. Feeding both into
one breaker means there is exactly one answer to "will the router send this
provider traffic".

**Rejected: no probes, cooldown only.** Half the size, and it makes `make
heal-apex` take as long as the current backoff regardless of the actual state of
the world. The interesting part of the demo would be a timer.

---

## The load generator runs in process

**Decision.** Traffic is generated inside the gateway, calling the router
directly rather than looping back through HTTP.

**Why.** Two reasons. The demo must have traffic without the viewer typing
anything, because the interesting moment is the failover and it should not have
to compete for attention with setup. And routing through a socket would put the
generator's own queueing, connection pooling and backpressure between the load
and the router, so a stall would be ambiguous: is that failover behavior or is
that the load generator?

Arrivals are exponentially distributed rather than evenly spaced. Evenly spaced
arrivals are the one traffic pattern that never occurs and the one that makes
rate limits look far kinder than they are — a token bucket sized for the mean
passes a metronome and refuses a Poisson stream at the same mean rate.

**Rejected: a separate load generator container.** More realistic in shape, and
it makes the compose file a four-service stack where one service exists to make
another service look busy. It also breaks `go run ./cmd/switchyard` as a way to
see the whole thing work.

**Rejected: no generator; document a curl loop.** The README would begin with
homework.

---

## Grafana is the interface

**Decision.** No custom frontend. The dashboards are checked-in JSON, provisioned
from disk, with UI updates disabled. Grafana runs with anonymous viewer access
and the Switchyard dashboard as its home page.

**Why.** The output of this project is a set of claims about routing behavior
under failure, and those claims are time series. Building a bespoke UI to display
time series that Grafana already displays would be a second project, and a worse
one — no ad-hoc querying, no time range picker, no correlation with anything
else, and a pile of frontend code between the reader and the data.

Dashboards as code, specifically: a dashboard clicked together in a browser and
exported is not reviewable, not diffable, and gone when the container is. The
panel that explains the failover should be readable in a pull request.

Anonymous access, specifically: the demo is three commands and a browser tab. A
login prompt in the middle of that is three commands and a password reset.

**Rejected: a small React app with a live incident timeline.** It was the first
idea, and it is scope creep with good taste. It would have taken longer than
everything else here combined and would have shown the same numbers less well.

**Rejected: exporting a dashboard from a running Grafana as the source of
truth.** Produces a 4,000-line JSON with UI state baked in, and every change is
an unreviewable diff.

---

## Tracing is instrumented but not exported by default

**Decision.** Spans are created throughout. The tracer provider is a no-op unless
`OTEL_EXPORTER_OTLP_ENDPOINT` is set, in which case OTLP export turns on with no
code change.

**Why.** A trace backend is a fourth service in the compose stack, and the
project's claims are all aggregate: request rates, failover counts, error budget
burn. Those are metrics. A trace of one request that failed over is a nice
artifact and not the argument.

Instrumenting anyway means the seam is there. Point it at a collector and traces
appear.

**Rejected: Tempo or Jaeger in the compose stack.** Another container, another
port, another thing to explain, in service of a view nobody needs to make the
point.

**Rejected: no tracing at all.** Then "OpenTelemetry throughout" would mean "the
metrics API", and adding tracing later would mean threading context through code
that was written without it.

---

## Metrics are OpenTelemetry, exported as Prometheus

**Decision.** Instruments are defined with the OTel metrics API and exported from
the gateway's own `/metrics` endpoint through the OTel Prometheus exporter.
Provider and breaker state are observable gauges read at scrape time.

**Why.** OTel because switching where telemetry goes should not mean rewriting
where it comes from. Prometheus exposition because the demo needs to work with
`docker compose up` and no collector.

Observable gauges rather than pushed values for breaker state because those
describe a condition that exists between requests rather than an event that
happened. Pushing them on each request would mean a provider carrying no traffic
has no state at all, which is precisely the case where you most want to know.

The latency histograms use explicit buckets spanning 10 ms to 30 s. The SDK
defaults top out well below that and would flatten the tail into a single bucket
exactly when the tail is the story.

---

## Providers are simulated, deterministically

**Decision.** Three simulated providers with lognormal latency, token-bucket rate
limits, concurrency caps and configurable error rates. Per-request randomness is
seeded from a hash of the request, so the same prompt draws the same latency and
the same reply every run.

**Why.** No API keys, no network egress, no spend, and no dependency on a third
party's uptime for a demo about third-party downtime. Determinism because a demo
that shifts under you teaches nothing, and a test that depends on a live provider
is a test that fails for reasons unrelated to the code.

Lognormal because that is the shape real request latency has: a firm floor, a
dense body, and a tail running well past the median. A uniform or normal
distribution would make p95 uninteresting.

State that genuinely depends on timing — the rate-limit bucket, the concurrency
count — is deliberately outside the deterministic seed, because those are
properties of the system under load rather than of the request.

**Rejected: real provider adapters behind a flag.** It would make the project
about API compatibility, which is tedious and already solved, rather than about
routing behavior. It also means anyone running the demo needs keys and a budget.

**Rejected: recorded fixtures from real providers.** Realistic latency, and
fixtures cannot be broken on demand, which is the entire point.

---

## The three providers make different tradeoffs on purpose

`apex` is fast, expensive and reliable. `bargain` is cheap, slow and
rate-limited. `local` is free and capacity-constrained.

A gateway choosing between equivalent upstreams is not making a decision worth
watching. The three-way tension is what makes both routing policies mean
something: `failover` prefers apex and pays for it; `cost` prefers local and
discovers that free capacity is finite. The local provider's slot limit is what
stops cost routing from pinning everything to a box that cannot hold it.

The default offered load is set below what bargain and local can carry between
them, so that breaking apex holds availability. This is not the demo being tuned
to look good — it is the demo being honest that failover is a capacity question.
`make spike-traffic` crosses that line deliberately and availability falls,
which is the more useful lesson.

---

## Identity enforcement is in two places

**Decision.** A `pre-push` hook checks every outgoing commit; a CI job checks
every commit in history.

**Why.** `core.hooksPath` is per-clone configuration. It does not survive a
clone, so a fresh clone has the hook file on disk and no hook installed. `make
init` installs it and is documented as step one, but any step a human performs is
a step a human forgets.

The hook is the fast local copy: it catches the mistake before it becomes
permanent, which matters because identity is baked into the commit hash and
GitHub's `refs/pull/N/head` is permanent. The CI job is the copy nobody can
forget to install.

**Rejected: hook only.** One forgotten `make init` and the guarantee is gone.

**Rejected: CI only.** By the time CI runs, the commit exists on a remote. For
identity, that is already too late.

Both check the same three things: author and committer on every commit, no
attribution strings in any commit message, and no attribution strings in any
tree at any commit — not just the tip, because a term introduced in one commit
and deleted in the next is still in the history a clone can read.

---

## Direct push, never a merge button

Documented in [CONTRIBUTING.md](CONTRIBUTING.md), and the short version is that
every server-side merge mode rewrites at least one identity field. Squash sets
the platform's `noreply` address as committer; rebase and merge stamp the
account identity. None produce the canonical identity, and the pre-push hook
cannot object because the platform performed the write, not the clone.
