# Roadmap

What v0.1 does not do, and what would have to be decided before it could.

This is a list of open questions, not a schedule. Items are here because the
design question is interesting and unanswered; an item leaves this file when it
is built or when it is ruled out for a reason worth writing down.

---

## v0.2 — streaming failover

**The headline, and the hard version of the problem v0.1 solves.**

v0.1's failover works because of a single ordering rule: the response header is
written only after some provider has accepted the request. Until the first byte
reaches the client, the gateway is free to try someone else, and the caller
cannot tell the difference. Every failure mode the demo shows — 503s, 429s,
capacity refusals — is surfaced from `Start`, before a stream exists.

That guarantee ends at the first token. A provider that dies forty tokens into a
two-hundred-token completion has already had its status line, its headers and
part of its body delivered on the gateway's behalf. There is no transparent
retry available: the client is holding a response that is both successful and
incomplete, and HTTP has no way to take it back.

So the question is not *how* to fail over mid-stream. It is **what a caller
should be handed when the gateway cannot.** Three candidate answers, none of them
free:

**Buffer and replay.** Hold the emitted tokens, and on a mid-stream failure
re-run the prompt against a second provider, discard the prefix it regenerates,
and splice the remainder onto what the client already has. Preserves the
illusion of one response. Costs a second full generation, and the splice is a
lie in a way the others are not: two models do not continue each other's text,
so the seam is a place where the response changes its mind. Also gives up the
latency that streaming exists to buy, if the buffer is held rather than forwarded.

**In-band error event.** Keep streaming, and when the upstream dies, emit a
terminal `error` event in the SSE stream and stop — which is what v0.1 already
does. Honest and cheap: the client is told, in the response it is already
reading, that the response is incomplete. Costs compatibility, because every
caller must now handle a stream that ends badly after starting well, and the
naive client that concatenates `chunk` events silently gets a truncated answer.

**Fail the stream.** Drop the connection and let the transport signal the
failure. Requires nothing of the protocol and nothing of the client library
beyond what it already handles. Loses the distinction between "the provider
died" and "the network did", which is exactly the distinction a gateway exists
to know.

The choice is a policy question, not an implementation one, and it probably
resolves to "per-route configuration, with a default that does not surprise
anyone". Naming it is the current state of the work.

---

## Known gaps

Not scheduled, and non-trivial for reasons worth stating.

**Retries.** v0.1 fails over between providers but never retries the same one.
Adding retry-on-timeout means deciding what a timeout actually proves: a request
that timed out may still be running upstream and may still complete, so a retry
risks paying for the same completion twice and — for the provider — serving it
twice. The gateway cannot tell an abandoned request from a slow one, which makes
"retry after N seconds" a billing decision wearing a reliability costume.

**Idempotency.** The honest fix for the above is an idempotency key, so a
retried request is recognized upstream as the same request rather than a new
one. That needs per-provider support: the key has to be accepted, stored and
honored by whoever is being retried, with an agreed retention window. The
simulated adapters have no such mechanism and inventing one for them would prove
nothing, because the hard part is not the gateway's side of the contract — it is
that real providers implement it inconsistently, or not at all.

---

## Not planned

The [Scope](README.md#scope) section of the README lists what v0.1 deliberately
excludes — no custom frontend, no real provider adapters, no LLM analysis layer,
no Kubernetes mode, no auth. Those are decisions rather than gaps, and nothing
above changes them.
