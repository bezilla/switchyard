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

### The caller giving up is not a failure kind either

The taxonomy above is about how a provider refuses. There is a fourth thing that
can end a request, and it is not the provider at all: the caller going away. A
client disconnects, or runs out of a deadline it set for itself, and the stream
dies mid-completion.

That is a failed request — the caller got no answer, and the SLO should say so —
but it is not evidence about the provider, and it must not push a circuit toward
open. Counting it is the same mistake as counting a 429: the upstream did
exactly what it was asked, and shunning it on that evidence keeps traffic away
from something that was working.

This was wrong until a real provider surfaced it, and the reason it hid for so
long is worth recording. No simulated provider is slow enough for anyone to give
up on. A real model on a CPU is: completions run past a minute, the load
generator's own thirty-second budget expired mid-stream, every abandoned
completion was recorded as a provider failure, and the circuit opened against an
upstream whose only fault was being slower than a deadline chosen for something
else. Roughly one request in seven, in steady state, with nothing broken.

The fix is one condition: the stream is marked failed only when the caller's
context is still live. An abandoned request now reports **nothing** to the
breaker — not a failure, and not a success either, because a spurious success
dilutes the failure ratio and would make a genuinely sick provider look
healthier the more callers gave up on it.

**How it was found, and how it is held.** It was found by measurement, not by
reading: a real model in the routing table produced roughly one stream error in
seven with nothing broken, and the counters said the provider was at fault. It
was confirmed by reverting the condition and watching twenty callers, each
timing out on its own deadline, open the circuit after eight requests and then
fail routing outright.

That reproduction is now a test rather than an afternoon. Restoring the old
classification fails three tests in `internal/router`: the two that assert a
canceled or expired caller leaves the breaker's failure count at zero, and the
aggregate one that drives twenty such callers and requires the circuit to stay
closed. Two further tests exist to catch the opposite mistake — a fix that
exempts too much — by requiring that a provider's *own* internal timeout, and a
connection dropped under a caller who is still waiting, both still count. Those
two pass against the old code as well, which is the point of having them.

The general lesson is worth more than the bug. A simulation fast enough to be
convenient is fast enough to hide this entire class of defect: no simulated
provider is slow enough for a caller to give up on one, so caller-cancellation
was never exercised until a real model was in the path. The simulated providers
remain the right default for everything they are good at, and this is the thing
they are structurally unable to test.

---

## Recovery is gradual

The three states, the admit ladder and the failure classification are drawn in
[the breaker state machine](docs/images/breaker-state-machine.svg); every number
on it is read from `DefaultConfig()` rather than illustrated.

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
outcomes. Probes always drive the breaker's view of an idle or avoided
provider; whether they may also cut an open circuit's cooldown short is a
separate, opt-in decision covered below.

**Why.** A breaker that learns only from traffic has a circularity problem: once
it opens, it stops sending traffic, and traffic was its only source of
information. It cannot notice that an idle provider died, and — worse for a
failover system — it cannot notice that a provider it is avoiding has recovered.
Time-based cooldowns paper over this by guessing. The probe is the out-of-band
signal that turns a guess into an observation.

What probes buy unconditionally is the ability to notice change at all: a
provider that died while idle, and a provider that recovered while shunned.
Whether a passing probe is strong enough evidence to *shorten a backoff* is a
different and much weaker claim, and it is made separately — see "Probe-driven
early recovery is off by default".

**Rejected: probes as a separate health signal from the breaker.** Two sources of
truth about whether a provider is usable, which then disagree. Feeding both into
one breaker means there is exactly one answer to "will the router send this
provider traffic".

**Rejected: no probes, cooldown only.** Half the size, and the breaker could
then never distinguish a provider that is still down from one that recovered
thirty seconds ago. It would retry blind, on a timer, forever.

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

**Partly reversed: real provider adapters behind a flag.** The original argument
was that adapters would make this a project about API compatibility rather than
about routing, and that anyone running the demo would need keys and a budget.
The first half still holds for *per-vendor* adapters and there are none. The
second half was answered by a model running on the reader's own machine: one
generic OpenAI-compatible adapter, opt in, no key. See "One adapter, not one per
vendor" below. The simulated three stay the default, unchanged, for the reason
above — nothing else can be broken on demand.

**Rejected: recorded fixtures from real providers.** Realistic latency, and
fixtures cannot be broken on demand, which is the entire point.

---

## One adapter, not one per vendor

**Decision.** There is a single `provider.Upstream` that speaks the OpenAI chat-
completions wire format over HTTP, configured by a base URL and a model name.
Ollama is its first consumer and has no special case anywhere in the code.

**Why.** Ollama, vLLM, LocalAI, llama.cpp's server and most hosted gateways all
expose the same two routes. One adapter reaches all of them, and adding the next
one is a config entry rather than a Go file. A vendor-specific adapter would be
the first of an unbounded set, each with its own quirks to keep working, which is
exactly the "project about API compatibility" this set out not to be.

**Rejected: an Ollama adapter, with generalization later.** It is one file
either way, and the generic version is the one that makes the second endpoint
free. It also keeps the demo honest: the interesting claim is that the router
does not know which of its providers do network I/O, and an adapter named after
one vendor invites the reader to assume otherwise.

**Rejected: putting the API key in the config file.** `api_key_env` names an
environment variable instead. A config file is a thing people commit, and a
secret scanner is a poor substitute for there being nowhere to put the secret.

---

## Start waits for the first token, over the network

**Decision.** `Upstream.Start` issues the request, checks the status, and then
reads forward to the first content token before returning a `Stream`. The token
is buffered and handed back on the first `Next`.

**Why.** The failover boundary is "every way a provider can refuse must surface
from `Start`". Over a network that is harder than it sounds, because the most
common way real inference degrades is not a 503 — it is a 200, headers out, and
then nothing. A saturated model server accepts the connection immediately and
thinks for forty seconds. If `Start` returned as soon as the status line looked
good, that would become a stream that hangs: the header naming the provider is
already out, the request is committed, and the gateway has lost the ability to
reroute exactly when it most needed it.

Reading to the first token buys the distinction, and it costs nothing that was
not going to be spent: time to first token is time to first token whether it is
spent inside `Start` or inside the first `Next`. What changes is only whether
the request is still reroutable while it elapses.

Past that token the stream is judged on the gap between tokens instead. A stall
there is a failed request rather than a reroutable one — the same rule the
simulated providers live under, and the reason streaming failover is a v0.3
question rather than a shipped feature.

**Rejected: a client-wide HTTP timeout.** One deadline cannot mean both "you had
long enough to start" and "you had long enough to finish". A completion
legitimately runs for minutes; a first token legitimately does not. A single
timeout large enough for the first is useless for the second.

**Rejected: returning the stream immediately and classifying the stall in
`Next`.** Simpler, and it moves the failure to the side of the line where
nothing can be done about it.

---

## A real provider needs a concurrency cap more than a rate limit

**Decision.** `max_concurrent` on an upstream, defaulting to 2 in the shipped
config, enforced before the request is sent. Exceeding it is `KindCapacity`:
fails over, does not count against health.

**Why.** This is the one setting that most changes how a local model behaves in
a routing table. A hosted API is elastic and pushes back with 429s; a model on
one machine is a fixed-size box, and admitting ten concurrent requests to it does
not make it serve ten — it makes all ten slow. That is the same shape as the
simulated `local` provider's six slots, which is not a coincidence: the simulated
provider was modeled on this case before there was a real one to check it
against.

Checking the cap before the network matters too. A refusal that costs a round
trip is a refusal that made the outage slightly worse.

**Rejected: queue instead of refuse.** A queue in front of a full box converts a
capacity problem into a latency problem, and hides it from the router, which is
the one component positioned to send the request somewhere with room.

---

## The OpenAI-compatible endpoint does not stream, and says so

**Decision.** `POST /v1/chat/completions` accepts the OpenAI request shape and
returns the OpenAI response shape, non-streaming. A request carrying
`"stream": true` is refused with a 400 naming `POST /v1/chat`, which does stream.

**Why ship half of it at all.** The value of "OpenAI-compatible" is that an
existing client can be pointed at the gateway by changing a base URL. Most
programmatic use — evaluations, batch jobs, anything that parses the result
before showing it to anyone — does not need tokens as they are made. That half is
worth shipping on its own.

**Why the explicit refusal is the whole argument.** The danger of a half-
compatible endpoint is not the missing half; it is a missing half that is silent.
A caller that sends `stream: true`, receives one JSON object at the end, and gets
no error has been misled about latency, about memory, and about which of the
gateway's guarantees applied to its request. It will find out in production. An
explicit 400 costs that caller one clear message and no illusions, and it is
also honest about the state of the work: streaming here waits on the same
question [ROADMAP.md](ROADMAP.md) has to answer for v0.3.

**Rejected: silently ignoring `stream`.** The failure mode above.

**Rejected: not shipping the endpoint until it streams.** The non-streaming half
is independently useful and its behavior is fully specified. Withholding it buys
nothing except a smaller changelog.

**Rejected: retrying mid-completion on this endpoint because it buffers anyway.**
Tempting: nothing has been written to the client, so a provider that dies at
token forty could be re-run elsewhere and nobody would know. It was rejected
because it would give this endpoint a different reliability contract from
`/v1/chat` — better on paper, and a second behavior to document, test, and
reason about on a dashboard that cannot tell the two endpoints apart. Uniform
routing was worth more than the wider guarantee on one route.

---

## Third-party references are pinned to immutable identifiers

**Decision.** GitHub Actions by full commit SHA, container images by digest,
`govulncheck` and `golangci-lint` by version. Renovate keeps the pins current
and is configured to open no branches and no pull requests.

**Why.** A tag is a mutable pointer in somebody else's repository. `actions/
checkout@v5` resolves to whatever its owner last moved `v5` to, which means a
compromised or merely changed action lands here on the next run with no diff in
this repository to explain it. Same for `prom/prometheus:v3.7.3`, which can be
repushed, and for `govulncheck@latest`, which can turn a green build red between
two runs of the same commit — indistinguishable from a real finding until
someone goes looking for a diff that does not exist.

The tag is kept in a comment beside every pin, because a bare digest tells a
reader nothing about what it is.

**Why Renovate writes only an issue.** Pinning is the easy half; a pin nobody
updates is a pin that is three advisories old. But a bot that opens a pull
request creates `refs/pull/N/head`, which GitHub keeps permanently whether the
pull request is merged, closed or deleted. `dependencyDashboardApproval` makes
"no branches" structural rather than a promise: nothing is created until a
checkbox is ticked, and the workflow is to read the dashboard and apply the
change by hand.

**Rejected: Dependabot.** Same ref-namespace cost, and no equivalent of the
approval-gated dashboard.

**Rejected: pinning without a bot.** That is the state this was in, and it is
the state where pins quietly rot.

---

## The three providers make different tradeoffs on purpose

`apex` is fast, expensive and reliable. `bargain` is cheap, slow and
rate-limited. `local` is free and capacity-constrained.

A gateway choosing between equivalent upstreams is not making a decision worth
watching. The three-way tension is what makes both routing policies mean
something: `failover` prefers apex and pays for it; `cost` prefers local and
discovers that free capacity is finite. The local provider's slot limit is what
stops cost routing from pinning everything to a box that cannot hold it.

---

## The default load sits under the surviving capacity, on purpose

**Decision.** The load generator defaults to 10 requests per second. With apex
broken, bargain (14 req/s sustained) and local (6 concurrent slots, about 3
req/s) can carry that between them with room left over, so `make break-apex`
holds availability at 100%.

**Why this is a decision and not a coincidence.** The first version ran at 25
req/s. Breaking apex there dropped 26 requests, because the surviving providers
could serve about 17 req/s between them and 25 were arriving. The failover
worked perfectly. Availability fell anyway.

There were two ways to make the demo green. Raise bargain's rate limit and
local's slot count until the numbers worked, or lower the offered load to
something the survivors could actually carry. The first is tuning the system
until the demo looks good. The second is stating a capacity budget and living
inside it.

**Failover is a capacity question wearing a reliability costume.** A router that
reroutes perfectly into capacity that does not exist has not preserved anything;
it has converted a provider outage into a queue. The interesting number was
never "did traffic move" — traffic always moves — it is "was there anywhere for
it to go". Every real failover plan is a claim about headroom on the fallback
path, and that claim is usually the part nobody checked.

So the default is the largest load the surviving providers can carry, and the
demo is honest about which fact it is demonstrating. `make spike-traffic` raises
the offered load to 45 req/s specifically so that combining it with
`make break-apex` makes availability fall. That is not the demo breaking. It is
the demo making its second point: the routing logic is unchanged, every failover
still fires, and availability drops anyway, because the capacity was not there.

**Rejected: raise the provider limits until 25 req/s survives.** It produces a
greener screenshot and a false lesson. Anyone who took this design to a real
system with a fallback sized like this one would discover the gap during an
incident instead of during a demo.

**Rejected: leave the default at 25 and let it drop requests.** Honest, and it
buries the first point under the second. Someone running `make break-apex` for
the first time should see failover work before they see its limit.

**Rejected: scale the fallback automatically to match offered load.** That is
autoscaling, and it is a different project. It also assumes elastic capacity,
which is exactly what the local provider is there to deny.

---

## Probe-driven early recovery is off by default

**Decision.** Two consecutive health probes passing against an open circuit can
cut the remaining cooldown short and start the recovery ramp immediately. This
is behind `-probe-early-recovery` (env `SWITCHYARD_PROBE_EARLY_RECOVERY`) and
defaults to **off**. `make heal-apex` turns it on through `POST /admin/recovery`
before clearing the fault.

**Why it exists.** After repeated failed recovery attempts the backoff reaches
tens of seconds, capped at sixty. That backoff was sized by an outage that has
since ended. When the provider is answering again, an operator watching a
dashboard waits out a timer computed from failures that stopped happening. In a
demo this is fatal: the interesting part of `make heal-apex` becomes a forty-
second pause.

**Why it is off anyway.** A health probe is a weaker signal than it looks. It is
cheap and shallow; a real inference request is neither. A dependency whose
connection pool is exhausted, whose cache is cold, or whose own downstream is
still down can answer a probe correctly while failing every real request it is
handed.

That gap turns into a flap. Two probes pass, a forty-second backoff collapses to
about one second, the ramp admits traffic, the traffic fails, the circuit
reopens with a longer cooldown — which the next two passing probes will shorten
again. The oscillation period stops being set by the backoff, which exists
precisely to damp this, and starts being set by the probe interval. The breaker
ends up amplifying the failure it was installed to contain.

The backoff is the conservative default because waiting is cheap when there is
somewhere else to send the traffic, and a gateway with a working failover path
always has somewhere else. Trading a slow heal for a stable one is the right
trade when the alternative provider is already carrying the load successfully.

Requiring two consecutive probes rather than one is a mitigation, not a fix. A
dependency that answers probes but cannot serve traffic passes two just as
easily as one.

**Enable it when** the probe exercises the same path a real request takes —
same connection pool, same downstream, comparable cost. Then it is measuring
what it claims to measure and the argument above does not apply.

**Rejected: on by default because the demo needs it.** Shipping a default whose
justification is "it makes the screenshot faster" is how a demo affordance
becomes a production incident. The demo can ask for it explicitly; the default
should be the one you would want at three in the morning.

**Rejected: drop the feature and let the demo be slow.** The heal is one of the
three commands in the README, and a forty-second dead pause in the middle of it
means nobody watches to the end.

**Rejected: shrink the backoff cap instead.** It would speed up the heal by
weakening the damping for every provider all the time, which is the same trade
made worse — permanent, and applied where it is not wanted.

---

## The e2e test asserts on rates, not counts

**Decision.** The end-to-end test judges the failover by comparing apex's
request rate during the outage against its own steady-state rate, requiring it
to fall below 15%. It does not assert an absolute number of requests.

**Why.** The first version capped apex at 15 requests after the fault. A run
landed 16 and failed, while demonstrating exactly the behavior under test. Those
sixteen were requests already in flight when the fault arrived — roughly offered
load times mean duration — a number that moves with the configured rps, with the
provider's latency distribution, and with how heavily loaded the CI runner is.

An absolute threshold there encodes an assumption about machine speed into a
correctness assertion. It fails for reasons unrelated to the code, and the usual
response to a flaky threshold is to loosen it until it stops failing, at which
point it no longer tests anything.

The rate ratio is what "traffic moved off apex" actually means, and it is
scale-independent: it stays valid if the default load changes, if the providers
get faster, or if the runner is slow. The observed value is 1-2% against a 15%
ceiling -- 2% on the two most recent runs, one local and one in CI -- so there is
real headroom without the assertion being vacuous.

**Rejected: raise the absolute cap to 40.** Same brittleness, postponed. The next
change to the default load reintroduces it.

**Rejected: assert only that bargain's count rose.** It would pass if apex kept
serving at full rate alongside bargain, which is not failover — it is
duplication.

## The commit gate is in two places

**Decision.** A `pre-push` hook checks every outgoing commit; a CI job checks
every commit in history. They enforce the same rules.

**Why.** `core.hooksPath` is per-clone configuration. It does not survive a
clone, so a fresh clone has the hook file on disk and no hook installed. `make
init` installs it and is documented as step one, but any step a human performs is
a step a human forgets.

The hook is the fast local copy: it catches the mistake before it becomes
permanent, which matters because identity is baked into the commit hash and
GitHub keeps `refs/pull/N/head` forever. The CI job is the copy nobody can
forget to install.

**Rejected: hook only.** One forgotten `make init` and the guarantee is gone.

**Rejected: CI only.** By the time CI runs, the commit exists on a remote. For
anything baked into a commit hash, that is already too late.

The consequence for how changes land — direct push, never the merge button — is
in [CONTRIBUTING.md](CONTRIBUTING.md), and the operational detail is in
[docs/maintainer-notes.md](docs/maintainer-notes.md).

**How the two stay in step.** Both files carry the same `check_trailers()`
function, byte for byte, and `.githooks/selftest.sh` hashes it out of each and
fails if they differ. Separate files rather than a shared library because they do
different jobs — the hook fails fast on the first problem in a push range, the CI
script reports counts for every scan over all history — and a library would trade
the duplication for a path dependency between `.githooks/` and `scripts/`. The
hashed-function test buys the same guarantee without the coupling.

## The gate allowlists trailers instead of hunting for names

**Decision.** Only three trailer keys may appear on a commit, or in an annotated
tag's body: `Signed-off-by` carrying exactly `Paul Bezilla
<bezilla@protonmail.com>`, `Verified` and `Measured` carrying free text.
Everything else is refused.

**Why.** What this replaced was two scans for a list of vendor names plus, in
`check-identity.sh`, a trailer **denylist** — `grep -icE
'generated|assisted|on-behalf-of'`. Measured before removing them: across the
full history of all six repositories in this family, 207 commits, the name scans
matched nothing, and the denylist counted nothing here.

A denylist catches the words somebody thought of. It is stale the day a tool
ships using a fourth one, and it cannot be made complete because the list of
things that do not exist yet is not enumerable. An allowlist inverts the
question: any tool that stamps provenance onto a commit does it through a
trailer, so an unlisted key is refused whether or not this repository has heard
of the thing that wrote it.

**Rejected: keeping the denylist and adding to it.** That is the same bet with a
longer list, and it loses on the first tool nobody predicted.

**Trailers are read with `git interpret-trailers --parse`, not a regex.** That is
git's own definition — the last paragraph, and only when the whole paragraph
parses as trailers — and it is the definition the tools stamping provenance use.
It has an edge worth stating: **whether a `Key: Value` line is a trailer depends
on which paragraph it lands in.** `Verified: ...` followed by more prose is
ordinary text the gate never inspects; the same line at the end is a trailer
whose key must be allowlisted. Six lines in this repository are prose of exactly
that shape — `hold:`, `load:`, `claim:`, `step:`, `one:` and, yes, `Verified:` —
so a `^Key:` regex would have rejected this repository's own history.

**Annotated tags are checked now**, which nothing did before: the tagger must be
the canonical identity, and the annotation body goes through the same allowlist.
Both `v0.1.0` and `v0.2.0` pass as they stand.

**Scope did not change.** `--branches --tags`, deliberately not `--all`. The
three `refs/pull/N/head` refs carrying dependabot's identity stay out of scope
for the reason recorded above: that identity exists on no branch and no tag here,
and a gate that flags a commit nobody can remove is a gate that gets switched
off.

**History was not rewritten.** No force push, no retag, nothing dropped. Both
gates were run over all 38 commits and both tags before the change landed: the
old hook accepted 38 and rejected 0, the new hook accepted 38 and rejected 0, and
the count of commits the old gate accepts and the new one refuses is 0.
