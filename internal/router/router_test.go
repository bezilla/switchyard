package router

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/provider"
)

// fakeProvider is a provider whose every behavior the test dictates. The
// simulated providers in package provider are deliberately not used here: they
// have latency distributions and baseline error rates, and a routing test that
// depends on either is a routing test that flakes.
type fakeProvider struct {
	name    string
	rates   provider.Rates
	startFn func() (provider.Stream, error)
	probeFn func() error

	starts int
}

func (f *fakeProvider) Name() string          { return f.name }
func (f *fakeProvider) Rates() provider.Rates { return f.rates }
func (f *fakeProvider) Probe(context.Context) error {
	if f.probeFn != nil {
		return f.probeFn()
	}
	return nil
}

func (f *fakeProvider) Start(context.Context, provider.Request) (provider.Stream, error) {
	f.starts++
	if f.startFn != nil {
		return f.startFn()
	}
	return &fakeStream{}, nil
}

// ok builds a provider that always serves.
func ok(name string, rates provider.Rates) *fakeProvider {
	return &fakeProvider{name: name, rates: rates}
}

// refusing builds a provider that always refuses with the given kind.
func refusing(name string, kind provider.FailureKind, rates provider.Rates) *fakeProvider {
	return &fakeProvider{
		name:  name,
		rates: rates,
		startFn: func() (provider.Stream, error) {
			return nil, &provider.Failure{Provider: name, Kind: kind, Message: "refused by test"}
		},
	}
}

type fakeStream struct {
	chunks   int
	emitted  int
	failWith error
	closed   bool
}

func (s *fakeStream) Next(context.Context) (provider.Chunk, error) {
	if s.failWith != nil {
		return provider.Chunk{}, s.failWith
	}
	if s.emitted >= s.chunks {
		return provider.Chunk{}, io.EOF
	}
	s.emitted++
	return provider.Chunk{Text: "x", Index: s.emitted - 1}, nil
}

func (s *fakeStream) Usage() provider.Usage { return provider.Usage{CompletionTokens: s.emitted} }
func (s *fakeStream) Close() error          { s.closed = true; return nil }

// target wraps a provider with a fresh breaker.
func target(p provider.Provider, priority int) *Target {
	return &Target{Provider: p, Priority: priority, Breaker: breaker.New(breaker.DefaultConfig())}
}

// recordingObserver captures events so tests can assert on what a dashboard
// would have been told, not only on what was returned.
type recordingObserver struct {
	attempts  []Attempt
	decisions []Decision
	failovers [][2]string
}

func (o *recordingObserver) Attempted(a Attempt) { o.attempts = append(o.attempts, a) }
func (o *recordingObserver) Decided(d Decision)  { o.decisions = append(o.decisions, d) }
func (o *recordingObserver) FailedOver(from, to string) {
	o.failovers = append(o.failovers, [2]string{from, to})
}

var req = provider.Request{Model: "default", Prompt: "hello world", MaxTokens: 100}

func TestFailoverPrefersLowestPriority(t *testing.T) {
	primary := ok("primary", provider.Rates{PromptUSDPerMTok: 100, CompletionUSDPerMTok: 100})
	secondary := ok("secondary", provider.Rates{})

	// The expensive provider is first in priority, and cheap alternatives are
	// available. Under failover, price is not a consideration.
	r := New(PolicyFailover, nil, target(primary, 10), target(secondary, 20))

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if d.Provider != "primary" {
		t.Fatalf("served by %q, want %q", d.Provider, "primary")
	}
	if d.Failovers != 0 {
		t.Fatalf("failovers = %d, want 0 when the primary serves", d.Failovers)
	}
	if secondary.starts != 0 {
		t.Fatalf("secondary was started %d times; it should not have been tried", secondary.starts)
	}
}

func TestFailoverMovesToNextOnRefusal(t *testing.T) {
	primary := refusing("primary", provider.KindUnavailable, provider.Rates{})
	secondary := ok("secondary", provider.Rates{})
	obs := &recordingObserver{}

	r := New(PolicyFailover, obs, target(primary, 10), target(secondary, 20))

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if d.Provider != "secondary" {
		t.Fatalf("served by %q, want %q", d.Provider, "secondary")
	}
	if d.Failovers != 1 {
		t.Fatalf("failovers = %d, want 1", d.Failovers)
	}
	if len(d.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: %+v", len(d.Attempts), d.Attempts)
	}
	if d.Attempts[0].Outcome != OutcomeRefused || d.Attempts[0].Kind != string(provider.KindUnavailable) {
		t.Fatalf("first attempt = %+v, want a refusal of kind %q", d.Attempts[0], provider.KindUnavailable)
	}
	if d.Attempts[1].Outcome != OutcomeServed {
		t.Fatalf("second attempt = %+v, want served", d.Attempts[1])
	}
	if len(obs.failovers) != 1 || obs.failovers[0] != [2]string{"primary", "secondary"} {
		t.Fatalf("observed failovers = %v, want one primary->secondary hop", obs.failovers)
	}
}

func TestFailoverWalksTheWholeChain(t *testing.T) {
	a := refusing("a", provider.KindUnavailable, provider.Rates{})
	b := refusing("b", provider.KindRateLimited, provider.Rates{})
	c := ok("c", provider.Rates{})

	r := New(PolicyFailover, nil, target(a, 10), target(b, 20), target(c, 30))

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if d.Provider != "c" {
		t.Fatalf("served by %q, want %q", d.Provider, "c")
	}
	if d.Failovers != 2 {
		t.Fatalf("failovers = %d, want 2", d.Failovers)
	}
}

func TestNoProviderWhenAllRefuse(t *testing.T) {
	a := refusing("a", provider.KindUnavailable, provider.Rates{})
	b := refusing("b", provider.KindCapacity, provider.Rates{})

	r := New(PolicyFailover, nil, target(a, 10), target(b, 20))

	_, d, err := r.Route(context.Background(), req)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("error = %v, want %v", err, ErrNoProvider)
	}
	if d.Provider != "" {
		t.Fatalf("decision names provider %q on total failure", d.Provider)
	}
	if len(d.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: every candidate should be recorded even "+
			"when none serve", len(d.Attempts))
	}
}

func TestRouterSkipsOpenBreaker(t *testing.T) {
	primary := ok("primary", provider.Rates{})
	secondary := ok("secondary", provider.Rates{})

	tp := target(primary, 10)
	r := New(PolicyFailover, nil, tp, target(secondary, 20))

	// Force the primary's circuit open.
	for range 10 {
		tp.Breaker.Failure()
	}
	if tp.Breaker.State() != breaker.Open {
		t.Fatalf("setup: breaker state = %v, want open", tp.Breaker.State())
	}

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if d.Provider != "secondary" {
		t.Fatalf("served by %q, want %q", d.Provider, "secondary")
	}
	if primary.starts != 0 {
		t.Fatalf("primary was started %d times; an open breaker means not even asking", primary.starts)
	}
	if d.Attempts[0].Outcome != OutcomeBreakerOpen {
		t.Fatalf("first attempt outcome = %q, want %q", d.Attempts[0].Outcome, OutcomeBreakerOpen)
	}
}

// TestRateLimitDoesNotCountAgainstHealth is the distinction the whole failure
// taxonomy exists for. A provider returning 429 is working; if 429s tripped its
// breaker, the breaker would then keep traffic away, the 429s would stop, and
// nothing would ever tell it to come back.
func TestRateLimitDoesNotCountAgainstHealth(t *testing.T) {
	limited := refusing("limited", provider.KindRateLimited, provider.Rates{})
	backup := ok("backup", provider.Rates{})

	tl := target(limited, 10)
	r := New(PolicyFailover, nil, tl, target(backup, 20))

	for range 20 {
		stream, _, err := r.Route(context.Background(), req)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		_ = stream.Close()
	}

	if got := tl.Breaker.State(); got != breaker.Closed {
		t.Fatalf("breaker state after 20 rate limits = %v, want %v", got, breaker.Closed)
	}
	if limited.starts != 20 {
		t.Fatalf("rate-limited provider was asked %d times, want 20: an open "+
			"breaker here would make the rate limit permanent", limited.starts)
	}
}

func TestUnavailableCountsAgainstHealth(t *testing.T) {
	broken := refusing("broken", provider.KindUnavailable, provider.Rates{})
	backup := ok("backup", provider.Rates{})

	tb := target(broken, 10)
	r := New(PolicyFailover, nil, tb, target(backup, 20))

	for range 20 {
		stream, _, err := r.Route(context.Background(), req)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		_ = stream.Close()
	}

	if got := tb.Breaker.State(); got != breaker.Open {
		t.Fatalf("breaker state after 20 unavailable errors = %v, want %v", got, breaker.Open)
	}
	if broken.starts >= 20 {
		t.Fatalf("broken provider was asked %d times; the breaker should have "+
			"stopped asking well before 20", broken.starts)
	}
}

func TestCostPolicyPicksCheapest(t *testing.T) {
	pricey := ok("pricey", provider.Rates{PromptUSDPerMTok: 3, CompletionUSDPerMTok: 15})
	cheap := ok("cheap", provider.Rates{PromptUSDPerMTok: 0.25, CompletionUSDPerMTok: 1.25})
	free := ok("free", provider.Rates{})

	// Priority deliberately favors the most expensive provider, so a pass
	// here cannot be priority ordering in disguise.
	r := New(PolicyCost, nil, target(pricey, 10), target(cheap, 20), target(free, 30))

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if d.Provider != "free" {
		t.Fatalf("served by %q, want %q", d.Provider, "free")
	}
	if d.Policy != PolicyCost {
		t.Fatalf("decision policy = %q, want %q", d.Policy, PolicyCost)
	}
}

func TestCostPolicyFailsOverToNextCheapest(t *testing.T) {
	pricey := ok("pricey", provider.Rates{PromptUSDPerMTok: 3, CompletionUSDPerMTok: 15})
	cheap := ok("cheap", provider.Rates{PromptUSDPerMTok: 0.25, CompletionUSDPerMTok: 1.25})
	free := refusing("free", provider.KindCapacity, provider.Rates{})

	r := New(PolicyCost, nil, target(pricey, 10), target(cheap, 20), target(free, 30))

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if d.Provider != "cheap" {
		t.Fatalf("served by %q, want %q: the free provider was full, so cost "+
			"routing should fall to the next cheapest", d.Provider, "cheap")
	}
	if d.Failovers != 1 {
		t.Fatalf("failovers = %d, want 1", d.Failovers)
	}
}

func TestCostPolicyBreaksTiesOnPriority(t *testing.T) {
	second := ok("second", provider.Rates{})
	first := ok("first", provider.Rates{})

	// Both free. Without a tiebreak the order would depend on sort internals,
	// and the routing would be unreproducible between runs.
	r := New(PolicyCost, nil, target(second, 20), target(first, 10))

	for range 5 {
		stream, d, err := r.Route(context.Background(), req)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		_ = stream.Close()
		if d.Provider != "first" {
			t.Fatalf("served by %q, want %q on every run", d.Provider, "first")
		}
	}
}

func TestSetPolicyChangesRouting(t *testing.T) {
	pricey := ok("pricey", provider.Rates{PromptUSDPerMTok: 3, CompletionUSDPerMTok: 15})
	cheap := ok("cheap", provider.Rates{PromptUSDPerMTok: 0.25, CompletionUSDPerMTok: 1.25})

	r := New(PolicyFailover, nil, target(pricey, 10), target(cheap, 20))

	stream, d, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	_ = stream.Close()
	if d.Provider != "pricey" {
		t.Fatalf("under failover, served by %q, want %q", d.Provider, "pricey")
	}

	if err := r.SetPolicy(PolicyCost); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	stream, d, err = r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	_ = stream.Close()
	if d.Provider != "cheap" {
		t.Fatalf("under cost, served by %q, want %q", d.Provider, "cheap")
	}
}

func TestSetPolicyRejectsUnknown(t *testing.T) {
	r := New(PolicyFailover, nil, target(ok("a", provider.Rates{}), 10))
	if err := r.SetPolicy(Policy("cheapest-on-tuesdays")); err == nil {
		t.Fatal("SetPolicy accepted an unknown policy")
	}
	if got := r.Policy(); got != PolicyFailover {
		t.Fatalf("policy = %q after a rejected change, want %q", got, PolicyFailover)
	}
}

func TestUnknownPolicyFallsBackToFailover(t *testing.T) {
	r := New(Policy("nonsense"), nil, target(ok("a", provider.Rates{}), 10))
	if got := r.Policy(); got != PolicyFailover {
		t.Fatalf("policy = %q, want %q", got, PolicyFailover)
	}
}

// TestStreamSuccessClosesTheHealthLoop checks that a completed stream feeds the
// breaker. Without this the breaker only ever hears about failures, and a
// provider that recovers under load never gets credit for it.
func TestStreamSuccessRecordsHealth(t *testing.T) {
	p := &fakeProvider{
		name:    "p",
		startFn: func() (provider.Stream, error) { return &fakeStream{chunks: 3}, nil },
	}
	tp := target(p, 10)
	r := New(PolicyFailover, nil, tp)

	stream, _, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if _, _, err := provider.Drain(context.Background(), stream); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if got := tp.Breaker.Stats().Successes; got != 1 {
		t.Fatalf("breaker successes = %d, want 1", got)
	}
}

// TestMidStreamFailureRecordsHealth covers the provider that accepts a request
// and then dies. Counting that as a success because Start worked would hide a
// genuinely broken upstream.
func TestMidStreamFailureRecordsHealth(t *testing.T) {
	boom := errors.New("upstream hung up")
	p := &fakeProvider{
		name: "p",
		startFn: func() (provider.Stream, error) {
			return &fakeStream{chunks: 3, failWith: boom}, nil
		},
	}
	tp := target(p, 10)
	r := New(PolicyFailover, nil, tp)

	stream, _, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if _, _, err := provider.Drain(context.Background(), stream); !errors.Is(err, boom) {
		t.Fatalf("Drain error = %v, want %v", err, boom)
	}

	stats := tp.Breaker.Stats()
	if stats.Failures != 1 || stats.Successes != 0 {
		t.Fatalf("breaker stats = %+v, want exactly one failure", stats)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	p := &fakeProvider{name: "p", startFn: func() (provider.Stream, error) {
		return &fakeStream{chunks: 1}, nil
	}}
	tp := target(p, 10)
	r := New(PolicyFailover, nil, tp)

	stream, _, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	for range 3 {
		_ = stream.Close()
	}

	if got := tp.Breaker.Stats().Successes; got != 1 {
		t.Fatalf("breaker successes after 3 closes = %d, want 1", got)
	}
}

func TestTargetLookup(t *testing.T) {
	r := New(PolicyFailover, nil, target(ok("a", provider.Rates{}), 10))
	if got := r.Target("a"); got == nil || got.Name() != "a" {
		t.Fatalf("Target(\"a\") = %v, want the target named a", got)
	}
	if got := r.Target("missing"); got != nil {
		t.Fatalf("Target(\"missing\") = %v, want nil", got)
	}
}

func TestHealthCheckerFeedsBreaker(t *testing.T) {
	failing := &fakeProvider{
		name: "failing",
		probeFn: func() error {
			return &provider.Failure{Provider: "failing", Kind: provider.KindUnavailable, Message: "down"}
		},
	}
	tf := target(failing, 10)
	r := New(PolicyFailover, nil, tf)

	h := NewHealthChecker(r, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	h.Run(ctx)

	if got := tf.Breaker.State(); got != breaker.Open {
		t.Fatalf("breaker state after repeated failing probes = %v, want %v: "+
			"an idle provider that has died must still be noticed", got, breaker.Open)
	}
	if at, err := tf.LastProbe(); err == nil || at.IsZero() {
		t.Fatalf("LastProbe = (%v, %v), want a recorded failure", at, err)
	}
}

// TestHealthCheckerIgnoresRateLimits mirrors the request-path rule: a probe
// that comes back 429 says nothing about whether the provider is healthy.
func TestHealthCheckerIgnoresRateLimits(t *testing.T) {
	limited := &fakeProvider{
		name: "limited",
		probeFn: func() error {
			return &provider.Failure{Provider: "limited", Kind: provider.KindRateLimited, Message: "429"}
		},
	}
	tl := target(limited, 10)
	r := New(PolicyFailover, nil, tl)

	h := NewHealthChecker(r, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	h.Run(ctx)

	if got := tl.Breaker.State(); got != breaker.Closed {
		t.Fatalf("breaker state after repeated 429 probes = %v, want %v", got, breaker.Closed)
	}
}
