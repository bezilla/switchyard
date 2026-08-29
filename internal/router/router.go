// Package router chooses which provider serves a request, and chooses again
// when that provider will not.
//
// Two policies ship: failover, which walks a fixed priority order, and cost,
// which walks providers cheapest-first. Both share the same machinery, because
// the interesting part is not the ordering -- it is what happens when the
// preferred choice refuses, and that is identical either way.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/provider"
)

// Policy names an ordering strategy.
type Policy string

const (
	// PolicyFailover serves from the highest-priority healthy provider. The
	// classic primary-with-failover arrangement: predictable, and it keeps
	// traffic on the provider you chose for quality until that provider stops
	// working.
	PolicyFailover Policy = "failover"

	// PolicyCost serves from the cheapest healthy provider that can take the
	// request. Same failover behavior underneath; different opinion about
	// what "best" means when everything is up.
	PolicyCost Policy = "cost"
)

// Valid reports whether p is a policy the router implements.
func (p Policy) Valid() bool {
	return p == PolicyFailover || p == PolicyCost
}

// Target is one provider plus the health state the router keeps about it.
type Target struct {
	Provider provider.Provider

	// Priority orders PolicyFailover, lowest first.
	Priority int

	// Breaker gates admission. It is fed by two sources: the outcome of real
	// requests, and the periodic health probe. Probes matter because a
	// provider carrying no traffic generates no request outcomes, and a
	// breaker that can only learn from traffic can never notice that an idle
	// provider has died -- or that it has come back.
	Breaker *breaker.Breaker

	mu        sync.Mutex
	lastProbe error
	probedAt  time.Time
}

// Name is the target's provider name.
func (t *Target) Name() string { return t.Provider.Name() }

// setProbe records the most recent probe result.
func (t *Target) setProbe(err error, at time.Time) {
	t.mu.Lock()
	t.lastProbe = err
	t.probedAt = at
	t.mu.Unlock()
}

// LastProbe reports when the most recent probe ran and what it found.
func (t *Target) LastProbe() (time.Time, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.probedAt, t.lastProbe
}

// Outcome is why an attempt on a provider ended the way it did.
type Outcome string

const (
	// OutcomeServed means this provider started the stream.
	OutcomeServed Outcome = "served"

	// OutcomeBreakerOpen means the router did not even try: the circuit is
	// open.
	OutcomeBreakerOpen Outcome = "breaker_open"

	// OutcomeShed means the circuit is recovering and this request was not in
	// the admitted fraction. Distinct from breaker_open on purpose: it is the
	// signal that a ramp is in progress rather than an outage.
	OutcomeShed Outcome = "shed"

	// OutcomeRefused means the provider was asked and said no.
	OutcomeRefused Outcome = "refused"
)

// Attempt records one provider considered for one request.
type Attempt struct {
	Provider string        `json:"provider"`
	Outcome  Outcome       `json:"outcome"`
	Kind     string        `json:"kind,omitempty"`
	Err      string        `json:"error,omitempty"`
	Took     time.Duration `json:"took,omitempty"`
}

// Decision is the full record of how one request was routed. It exists so that
// routing is observable rather than inferred: the point of the project is that
// you can see the gateway change its mind.
type Decision struct {
	Policy Policy `json:"policy"`

	// Provider is who served it, empty if nobody did.
	Provider string `json:"provider"`

	// Attempts is every provider considered, in the order considered.
	Attempts []Attempt `json:"attempts"`

	// Failovers is how many providers refused before one served. Zero on a
	// normal request; this is the number that moves during an incident.
	Failovers int `json:"failovers"`
}

// ErrNoProvider is returned when every candidate refused or was gated out.
var ErrNoProvider = errors.New("no provider available")

// Observer receives routing events for telemetry. All methods must tolerate
// being called concurrently.
type Observer interface {
	// Attempted fires once per provider considered.
	Attempted(a Attempt)

	// Decided fires once per request, after routing resolves either way.
	Decided(d Decision)

	// FailedOver fires on each hop from a refusing provider to the next
	// candidate, which is the event a dashboard actually wants to count.
	FailedOver(from, to string)
}

// NopObserver ignores everything.
type NopObserver struct{}

// Attempted implements Observer.
func (NopObserver) Attempted(Attempt) {}

// Decided implements Observer.
func (NopObserver) Decided(Decision) {}

// FailedOver implements Observer.
func (NopObserver) FailedOver(string, string) {}

// Router routes requests across targets.
type Router struct {
	mu      sync.RWMutex
	targets []*Target
	policy  Policy

	obs Observer
}

// New builds a router. Targets are copied, so later mutation of the slice does
// not affect the router.
func New(policy Policy, obs Observer, targets ...*Target) *Router {
	if !policy.Valid() {
		policy = PolicyFailover
	}
	if obs == nil {
		obs = NopObserver{}
	}
	cp := make([]*Target, len(targets))
	copy(cp, targets)
	return &Router{targets: cp, policy: policy, obs: obs}
}

// Policy reports the active policy.
func (r *Router) Policy() Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policy
}

// SetPolicy switches policy at runtime, so the demo can show the same traffic
// routed two different ways without a restart.
func (r *Router) SetPolicy(p Policy) error {
	if !p.Valid() {
		return fmt.Errorf("unknown policy %q", p)
	}
	r.mu.Lock()
	r.policy = p
	r.mu.Unlock()
	return nil
}

// Targets returns the router's targets.
func (r *Router) Targets() []*Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Target, len(r.targets))
	copy(out, r.targets)
	return out
}

// Target looks up a target by provider name.
func (r *Router) Target(name string) *Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.targets {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// order ranks candidates for a request under the active policy.
func (r *Router) order(req provider.Request) ([]*Target, Policy) {
	r.mu.RLock()
	policy := r.policy
	out := make([]*Target, len(r.targets))
	copy(out, r.targets)
	r.mu.RUnlock()

	switch policy {
	case PolicyCost:
		// Cheapest first, priority breaking ties so the order is total and
		// therefore reproducible. Free providers sort to the front, which is
		// why the local provider's capacity limit matters: without it, cost
		// routing would pin everything to a box that cannot hold it.
		sort.SliceStable(out, func(i, j int) bool {
			ci := out[i].Provider.Rates().EstimateFor(req)
			cj := out[j].Provider.Rates().EstimateFor(req)
			if ci != cj {
				return ci < cj
			}
			return out[i].Priority < out[j].Priority
		})
	case PolicyFailover:
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].Priority < out[j].Priority
		})
	}
	return out, policy
}

// Route picks a provider and starts a stream on it, trying candidates in policy
// order until one serves the request.
//
// The returned Stream is wrapped: closing it reports the terminal outcome back
// to the chosen provider's breaker. Callers must Close it exactly as they would
// close a provider stream, or that provider's health signal goes stale.
func (r *Router) Route(ctx context.Context, req provider.Request) (provider.Stream, Decision, error) {
	candidates, policy := r.order(req)

	d := Decision{Policy: policy, Attempts: make([]Attempt, 0, len(candidates))}
	prev := ""

	record := func(a Attempt) {
		d.Attempts = append(d.Attempts, a)
		r.obs.Attempted(a)
	}

	for _, t := range candidates {
		name := t.Name()

		if prev != "" {
			r.obs.FailedOver(prev, name)
			d.Failovers++
		}

		if !t.Breaker.Allow() {
			out := OutcomeBreakerOpen
			if t.Breaker.State() == breaker.Recovering {
				out = OutcomeShed
			}
			record(Attempt{Provider: name, Outcome: out})
			prev = name
			continue
		}

		start := time.Now()
		stream, err := t.Provider.Start(ctx, req)
		took := time.Since(start)

		if err != nil {
			kind := provider.KindOf(err)
			// Load shedding is not ill health. A rate-limited provider that
			// counted 429s against itself would trip its own breaker, and
			// then stay tripped because the breaker kept traffic away, which
			// is the opposite of what a rate limit is asking for.
			if kind.CountsAgainstHealth() {
				t.Breaker.Failure()
			} else {
				t.Breaker.Success()
			}
			record(Attempt{
				Provider: name,
				Outcome:  OutcomeRefused,
				Kind:     string(kind),
				Err:      err.Error(),
				Took:     took,
			})
			prev = name
			continue
		}

		record(Attempt{Provider: name, Outcome: OutcomeServed, Took: took})
		d.Provider = name
		r.obs.Decided(d)
		return &gatedStream{Stream: stream, br: t.Breaker}, d, nil
	}

	r.obs.Decided(d)
	return nil, d, fmt.Errorf("%w: %d candidate(s) tried", ErrNoProvider, len(candidates))
}

// gatedStream reports the stream's terminal outcome to the breaker, so that a
// provider which accepts a request and then dies partway through is counted as
// unhealthy rather than as a success.
type gatedStream struct {
	provider.Stream
	br *breaker.Breaker

	mu     sync.Mutex
	failed bool
	done   bool
}

func (g *gatedStream) Next(ctx context.Context) (provider.Chunk, error) {
	chunk, err := g.Stream.Next(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		g.mu.Lock()
		g.failed = true
		g.mu.Unlock()
	}
	return chunk, err
}

func (g *gatedStream) Close() error {
	g.mu.Lock()
	first := !g.done
	g.done = true
	failed := g.failed
	g.mu.Unlock()

	if first {
		if failed {
			g.br.Failure()
		} else {
			g.br.Success()
		}
	}
	return g.Stream.Close()
}
