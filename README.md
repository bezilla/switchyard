# Switchyard

**AI infrastructure's production problems are the old problems in new vocabulary.**
Routing, failover, rate limits, quotas, cost control, observability — the same
concerns that CDNs and load balancers have handled for two decades, wearing
different words. Switchyard is that argument as running code: a gateway that
routes inference across providers, survives their failures, and shows you the
whole thing happening on a dashboard.

```
docker compose up          # Grafana on :3000, traffic already flowing
make break-apex            # apex goes down, traffic shifts, availability holds
make heal-apex             # gradual restoration, no stampede
```

```
                    ┌──────────────────────────────────────────────────┐
                    │                   switchyard                     │
   load generator   │                                                  │
   (traffic is  ────┼──▶  router ──▶ policy: failover | cost           │
    always on)      │       │                                          │
                    │       ├──▶ circuit breaker  ─── gradual recovery │
   POST /v1/chat ───┼──▶    │        ▲                                 │
                    │       │        │ health probes (out of band)     │
                    │       ▼        │                                 │
                    │   ┌───────┬────┴────┬─────────┐                  │
                    │   │ apex  │ bargain │  local  │  simulated       │
                    │   │ fast  │  cheap  │  free   │  providers       │
                    │   │ $$$   │  slow   │ capped  │                  │
                    │   │reliable│ 429s   │ 6 slots │                  │
                    │   └───────┴─────────┴─────────┘                  │
                    │                                                  │
                    │   OpenTelemetry ──▶ /metrics                     │
                    └───────────────────────┬──────────────────────────┘
                                            │ scrape
                                   Prometheus ──▶ Grafana :3000
```

---

## What it does

Three simulated providers, each making a different tradeoff:

| provider  | latency to first token | price | how it fails |
|-----------|------------------------|-------|--------------|
| `apex`    | ~90 ms                 | $3 / $15 per Mtok | rarely, and then completely |
| `bargain` | ~520 ms                | $0.25 / $1.25 per Mtok | 429s once past 14 req/s |
| `local`   | ~210 ms                | free | refuses once its 6 slots are full |

The router serves from the best available one and moves when it has to. Two
policies ship: `failover` walks a fixed priority order, `cost` walks providers
cheapest-first. Both share the same failover machinery, because the interesting
part is not the ordering — it is what happens when the preferred choice refuses.

**Failover is invisible to the caller.** Every way a provider can refuse work is
surfaced before a stream exists, so until the first byte goes out the gateway is
still free to change its mind about who serves the request. Once a token has been
written, it cannot.

**A 429 is not ill health.** A rate-limited provider is working correctly and
shedding our load. If 429s tripped its circuit breaker, the breaker would then
keep traffic away, the 429s would stop, and nothing would ever say it was safe to
come back. Rate limits and capacity refusals trigger failover without counting
against health; errors and timeouts count.

**Recovery is a ramp, not a cliff.** A textbook breaker's half-open state admits
one probe and then closes fully, handing a just-restarted provider one hundred
percent of the traffic it was failing under. This one admits 5% and grows
geometrically while the provider keeps succeeding, so the provider warms up under
load it can survive.

---

## The demo

```sh
make up            # or: docker compose up
```

Open <http://localhost:3000>. No login — the dashboard is the home page and
traffic is already flowing. Give it thirty seconds to fill in.

### `make break-apex`

Apex starts returning 503s. Within a couple of seconds its circuit opens and the
traffic band on the top-left panel collapses into bargain. Watch three things:

- **Traffic by provider** — the apex band vanishes, the bargain band rises to
  meet the same total. The total does not dip.
- **Availability** — stays at its objective. This is the claim.
- **Estimated spend rate** — drops, because bargain is a twelfth the price. The
  incident is cheaper than the steady state, which is its own kind of lesson.

Time to first token gets worse: apex answers in about 90 ms, bargain in about
520 ms. That is what the availability cost.

### `make heal-apex`

Apex answers again. The health probe notices within a second, and the breaker
starts a ramp rather than a return. On the **breaker admit ratio** panel the line
climbs 0.05, 0.08, 0.13, 0.21, 0.33, 0.52, 0.84 and then closes — about six
seconds of partial traffic before full. A vertical line there would be the
stampede this exists to avoid.

### The rest

```sh
make ratelimit-bargain   # a healthy provider shedding load; its breaker stays closed
make slow-apex           # 12x slower and still passing health checks
make spike-traffic       # 45 rps: find the edge of the failover capacity
make policy-cost         # route cheapest-first instead of primary-first
make state               # the gateway's current routing and health state, as JSON
make ask                 # send one request and watch the tokens stream
make reset               # everything back to healthy
make down                # stop
```

`make spike-traffic` combined with `make break-apex` is the honest one:
availability falls, because failover cannot conjure capacity the surviving
providers never had. The default load is deliberately below that line. Failure
handling is a capacity question wearing a reliability costume.

---

## Try it without the stack

```sh
go run ./cmd/switchyard              # gateway on :8080, traffic flowing
curl -N -X POST localhost:8080/v1/chat \
  -H 'content-type: application/json' \
  -d '{"prompt":"why does a gateway care about the difference between 429 and 503?"}'
```

The response streams as server-sent events, and the headers name the decision:

```
X-Switchyard-Provider: apex
X-Switchyard-Policy: failover
X-Switchyard-Failovers: 0
```

| endpoint | what it does |
|----------|--------------|
| `POST /v1/chat` | streams a completion; headers name the provider chosen |
| `GET /metrics` | Prometheus exposition |
| `GET /admin/state` | routing, breaker and health state as JSON |
| `POST /admin/inject` | `{"provider":"apex","mode":"error\|ratelimit\|slow\|healthy","rate":1}` |
| `POST /admin/policy` | `{"policy":"failover\|cost"}` |
| `POST /admin/traffic` | `{"rps":45}` |
| `GET /healthz`, `GET /readyz` | process liveness; whether any provider can serve |

---

## Development

```sh
make init      # step 1 for any clone: installs the pre-push gate
make check     # vet, lint, race tests, identity
make e2e       # start the stack, break a provider, assert from parsed metrics
```

`make e2e` is the test that matters. It starts the real stack, injects a real
failure, and asserts on numbers that must have changed — bargain served at least
N more requests than before, the failover counter moved, availability stayed
above its floor. A run where the gateway silently served nothing would satisfy
"no errors occurred"; it would not satisfy that.

See [DESIGN.md](DESIGN.md) for the decisions and the alternatives that were
rejected, and [CONTRIBUTING.md](CONTRIBUTING.md) before your first commit.

---

## Limitations

These matter, and glossing over them would undercut the point.

- **The providers are simulated, and simulated providers are not real ones.**
  They have no tokenizer, no model, no network, and no bad day nobody predicted.
  Their latency distributions and failure modes are chosen to be plausible, not
  measured. Real providers fail in ways this does not contain: partial
  degradation by region, silent quality drops, quota resets at surprising
  boundaries, capacity that varies by model and time of day.

- **Cost is estimated, not billed.** Prices are in the shape and rough magnitude
  of published pricing without being any vendor's numbers. Token counts come from
  a four-characters-per-token approximation that no real tokenizer agrees with.
  The cost panels are correct about direction and proportion and wrong about
  dollars.

- **This demonstrates routing behavior; it is not a production gateway.** There
  is no authentication, no multi-tenancy, no request persistence, no admission
  control beyond a concurrency bound, no retry budget, and no adapter for any
  real provider API. The admin endpoints that break things are unauthenticated
  by design, which alone should keep this off anything public.

- **The failover boundary is real but narrow.** A request is reroutable only
  before its first token. A provider that accepts a request and dies halfway
  through produces a failed request, and no gateway architecture changes that
  without buffering the entire completion and giving up streaming.

- **`docker compose up` is not a load test.** The default 10 requests per second
  is sized so the demo is legible, not so the numbers mean anything about
  throughput.

## License

MIT. See [LICENSE](LICENSE).
