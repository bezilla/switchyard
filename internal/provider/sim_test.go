package provider

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// closeTo compares dollar amounts. Cost is a sum of two floating-point
// products, so an expected value written in a different association order can
// differ in the last bit without either being wrong.
func closeTo(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// fast builds a provider with negligible latency so tests assert on behavior
// rather than spend their time sleeping.
func fast(cfg Config) *Sim {
	cfg.TTFT = Latency{Median: time.Microsecond}
	cfg.PerToken = Latency{Median: time.Microsecond}
	if cfg.MinReplyTokens == 0 {
		cfg.MinReplyTokens = 5
		cfg.MaxReplyTokens = 5
	}
	return New(cfg)
}

var ctx = context.Background()

func TestDeterministicForTheSameRequest(t *testing.T) {
	req := Request{Model: "default", Prompt: "the same prompt every time", MaxTokens: 500}

	first := ""
	var firstUsage Usage
	for i := range 5 {
		p := fast(Config{Name: "d", Seed: 7, MinReplyTokens: 20, MaxReplyTokens: 200})
		s, err := p.Start(ctx, req)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		text, usage, err := Drain(ctx, s)
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if i == 0 {
			first, firstUsage = text, usage
			continue
		}
		if text != first {
			t.Fatalf("run %d produced different text; the simulation is not reproducible", i)
		}
		if usage != firstUsage {
			t.Fatalf("run %d usage = %+v, want %+v", i, usage, firstUsage)
		}
	}
}

func TestDifferentPromptsDiffer(t *testing.T) {
	p := fast(Config{Name: "d", Seed: 7, MinReplyTokens: 20, MaxReplyTokens: 200})

	a, err := p.Start(ctx, Request{Prompt: "first prompt", MaxTokens: 500})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	textA, _, _ := Drain(ctx, a)

	b, err := p.Start(ctx, Request{Prompt: "an entirely different prompt", MaxTokens: 500})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	textB, _, _ := Drain(ctx, b)

	if textA == textB {
		t.Fatal("two different prompts produced identical completions")
	}
}

func TestUsageAndCost(t *testing.T) {
	p := fast(Config{
		Name:           "priced",
		Rates:          Rates{PromptUSDPerMTok: 3, CompletionUSDPerMTok: 15},
		MinReplyTokens: 10,
		MaxReplyTokens: 10,
	})

	// 40 characters, so 10 prompt tokens at four characters each.
	prompt := "0123456789012345678901234567890123456789"
	s, err := p.Start(ctx, Request{Prompt: prompt, MaxTokens: 100})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, usage, err := Drain(ctx, s)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if usage.PromptTokens != 10 {
		t.Fatalf("prompt tokens = %d, want 10", usage.PromptTokens)
	}
	if usage.CompletionTokens != 10 {
		t.Fatalf("completion tokens = %d, want 10", usage.CompletionTokens)
	}
	want := 10.0/1e6*3 + 10.0/1e6*15
	if !closeTo(usage.CostUSD, want) {
		t.Fatalf("cost = %v, want %v", usage.CostUSD, want)
	}
}

func TestMaxTokensCapsTheCompletion(t *testing.T) {
	p := fast(Config{Name: "d", MinReplyTokens: 500, MaxReplyTokens: 500})

	s, err := p.Start(ctx, Request{Prompt: "hello", MaxTokens: 7})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, usage, err := Drain(ctx, s)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if usage.CompletionTokens != 7 {
		t.Fatalf("completion tokens = %d, want 7", usage.CompletionTokens)
	}
}

func TestRateLimitRefusesOnceTheBucketEmpties(t *testing.T) {
	p := fast(Config{
		Name:  "limited",
		Limit: &RateLimit{RequestsPerSecond: 0.0001, Burst: 3},
	})

	// The burst allowance, then nothing.
	var open []Stream
	for i := range 3 {
		s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 1})
		if err != nil {
			t.Fatalf("request %d within burst was refused: %v", i, err)
		}
		open = append(open, s)
	}
	defer func() {
		for _, s := range open {
			_ = s.Close()
		}
	}()

	_, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 1})
	if err == nil {
		t.Fatal("request past the burst was allowed")
	}
	if got := KindOf(err); got != KindRateLimited {
		t.Fatalf("kind = %q, want %q", got, KindRateLimited)
	}

	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("error %v is not a *Failure", err)
	}
	if f.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want a positive hint", f.RetryAfter)
	}
	if f.Provider != "limited" {
		t.Fatalf("failure names provider %q, want %q", f.Provider, "limited")
	}
}

func TestRateLimitRefillsOverTime(t *testing.T) {
	p := fast(Config{
		Name:  "limited",
		Limit: &RateLimit{RequestsPerSecond: 100, Burst: 1},
	})

	s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 1})
	if err != nil {
		t.Fatalf("first request refused: %v", err)
	}
	_ = s.Close()

	if _, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 1}); err == nil {
		t.Fatal("second immediate request was allowed with a burst of 1")
	}

	// At 100 per second, one token is back in 10ms.
	time.Sleep(30 * time.Millisecond)
	s, err = p.Start(ctx, Request{Prompt: "x", MaxTokens: 1})
	if err != nil {
		t.Fatalf("request after refill was refused: %v", err)
	}
	_ = s.Close()
}

func TestCapacityRefusesWhenSlotsAreFull(t *testing.T) {
	p := fast(Config{Name: "boxed", MaxConcurrent: 2})

	a, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 100})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := p.Start(ctx, Request{Prompt: "y", MaxTokens: 100})
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if got := p.Inflight(); got != 2 {
		t.Fatalf("inflight = %d, want 2", got)
	}

	_, err = p.Start(ctx, Request{Prompt: "z", MaxTokens: 100})
	if err == nil {
		t.Fatal("third request was allowed past a cap of 2")
	}
	if got := KindOf(err); got != KindCapacity {
		t.Fatalf("kind = %q, want %q", got, KindCapacity)
	}

	// Closing a stream must return the slot, or the provider leaks capacity
	// until restart.
	_ = a.Close()
	if got := p.Inflight(); got != 1 {
		t.Fatalf("inflight after one close = %d, want 1", got)
	}
	c, err := p.Start(ctx, Request{Prompt: "z", MaxTokens: 100})
	if err != nil {
		t.Fatalf("request after a slot freed was refused: %v", err)
	}
	_ = b.Close()
	_ = c.Close()
}

func TestCloseIsIdempotentForCapacity(t *testing.T) {
	p := fast(Config{Name: "boxed", MaxConcurrent: 1})

	s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 10})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 5 {
		_ = s.Close()
	}
	if got := p.Inflight(); got != 0 {
		t.Fatalf("inflight after repeated closes = %d, want 0: a double close "+
			"must not hand out capacity that does not exist", got)
	}
}

func TestInjectedErrorFailsStartAndProbe(t *testing.T) {
	p := fast(Config{Name: "apex"})

	if err := p.Probe(ctx); err != nil {
		t.Fatalf("healthy probe: %v", err)
	}

	p.Inject(Injection{Mode: ModeError, Rate: 1})

	_, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 10})
	if err == nil {
		t.Fatal("Start succeeded against a fully broken provider")
	}
	if got := KindOf(err); got != KindUnavailable {
		t.Fatalf("kind = %q, want %q", got, KindUnavailable)
	}
	if err := p.Probe(ctx); err == nil {
		t.Fatal("probe succeeded against a fully broken provider")
	}
}

func TestInjectHealthyClearsTheFault(t *testing.T) {
	p := fast(Config{Name: "apex"})
	p.Inject(Injection{Mode: ModeError, Rate: 1})
	p.Inject(Injection{Mode: ModeHealthy})

	if err := p.Probe(ctx); err != nil {
		t.Fatalf("probe after healing: %v", err)
	}
	s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 10})
	if err != nil {
		t.Fatalf("Start after healing: %v", err)
	}
	_ = s.Close()
}

// TestSlowProviderStaysHealthy is the failure mode an up/down check misses. A
// provider answering ten times slower is not down, and reporting it as down
// would hide the thing an operator most needs to see.
func TestSlowProviderStaysHealthy(t *testing.T) {
	p := fast(Config{Name: "apex"})
	p.Inject(Injection{Mode: ModeSlow, SlowFactor: 20})

	if err := p.Probe(ctx); err != nil {
		t.Fatalf("probe against a slow provider = %v, want nil", err)
	}
	s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 5})
	if err != nil {
		t.Fatalf("Start against a slow provider: %v", err)
	}
	_ = s.Close()
}

func TestInjectedRateLimitIsNotIllHealth(t *testing.T) {
	p := fast(Config{Name: "bargain"})
	p.Inject(Injection{Mode: ModeRateLimit, Rate: 1})

	_, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 10})
	if got := KindOf(err); got != KindRateLimited {
		t.Fatalf("kind = %q, want %q", got, KindRateLimited)
	}
	if KindOf(err).CountsAgainstHealth() {
		t.Fatal("a rate limit was counted against provider health")
	}
}

func TestInjectDefaultsRateToFull(t *testing.T) {
	p := fast(Config{Name: "apex"})
	p.Inject(Injection{Mode: ModeError})

	if got := p.Injection().Rate; got != 1 {
		t.Fatalf("injected rate = %v, want 1: a fault with no rate means all "+
			"of it, not none of it", got)
	}
}

func TestPartialInjectionIsPartial(t *testing.T) {
	p := fast(Config{Name: "apex"})
	p.Inject(Injection{Mode: ModeError, Rate: 0.5})

	var failed, served int
	for i := range 200 {
		s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 1})
		if err != nil {
			failed++
			continue
		}
		served++
		_ = s.Close()
		_ = i
	}
	if failed == 0 || served == 0 {
		t.Fatalf("a 50%% injection produced %d failures and %d successes; it "+
			"should produce both", failed, served)
	}
}

func TestContextCancellationStopsAStream(t *testing.T) {
	p := New(Config{
		Name:           "slow",
		TTFT:           Latency{Median: 50 * time.Millisecond},
		PerToken:       Latency{Median: 50 * time.Millisecond},
		MinReplyTokens: 100,
		MaxReplyTokens: 100,
	})

	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()

	s, err := p.Start(cctx, Request{Prompt: "x", MaxTokens: 100})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Next(cctx); err == nil {
		t.Fatal("Next returned a chunk after the context expired")
	} else if KindOf(err) != KindTimeout {
		t.Fatalf("kind = %q, want %q", KindOf(err), KindTimeout)
	}
}

func TestNextAfterCloseIsAnError(t *testing.T) {
	p := fast(Config{Name: "d"})
	s, err := p.Start(ctx, Request{Prompt: "x", MaxTokens: 10})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = s.Close()

	if _, err := s.Next(ctx); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Next after Close = %v, want %v", err, ErrStreamClosed)
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	for prompt, want := range map[string]int{
		"":         1,
		"a":        1,
		"abcd":     1,
		"abcde":    2,
		"01234567": 2,
	} {
		if got := EstimatePromptTokens(prompt); got != want {
			t.Errorf("EstimatePromptTokens(%q) = %d, want %d", prompt, got, want)
		}
	}
}

func TestEstimateForIsPessimistic(t *testing.T) {
	r := Rates{PromptUSDPerMTok: 3, CompletionUSDPerMTok: 15}
	req := Request{Prompt: "0123456789012345678901234567890123456789", MaxTokens: 100}

	// The estimate must price the completion at MaxTokens, because assuming a
	// short answer would let cost routing pick an expensive provider on the
	// strength of a guess.
	want := 10.0/1e6*3 + 100.0/1e6*15
	if got := r.EstimateFor(req); !closeTo(got, want) {
		t.Fatalf("EstimateFor = %v, want %v", got, want)
	}
}

func TestFailureKindHealthClassification(t *testing.T) {
	for kind, want := range map[FailureKind]bool{
		KindUnavailable: true,
		KindTimeout:     true,
		KindRateLimited: false,
		KindCapacity:    false,
	} {
		if got := kind.CountsAgainstHealth(); got != want {
			t.Errorf("%q.CountsAgainstHealth() = %v, want %v", kind, got, want)
		}
	}
}

func TestKindAndNameOfNonProviderError(t *testing.T) {
	err := errors.New("something else entirely")
	if got := KindOf(err); got != KindUnavailable {
		t.Errorf("KindOf(non-provider error) = %q, want %q", got, KindUnavailable)
	}
	if got := NameOf(err); got != "" {
		t.Errorf("NameOf(non-provider error) = %q, want empty", got)
	}
}

func TestCatalogProvidersHaveTheirIntendedCharacter(t *testing.T) {
	apex, bargain, local := Apex(1), Bargain(1), Local(1)

	if apex.Rates().CompletionUSDPerMTok <= bargain.Rates().CompletionUSDPerMTok {
		t.Error("apex should cost more per completion token than bargain")
	}
	if local.Rates().CompletionUSDPerMTok != 0 || local.Rates().PromptUSDPerMTok != 0 {
		t.Errorf("local should be free, got %+v", local.Rates())
	}
	if local.Capacity() <= 0 {
		t.Error("local should be capacity-constrained")
	}
	if apex.Capacity() != 0 {
		t.Error("apex should not be capacity-constrained")
	}
	// Bargain's limit is the thing that makes failing over to it interesting.
	if bargain.cfg.Limit == nil {
		t.Error("bargain should be rate-limited")
	}
	if apex.cfg.TTFT.Median >= bargain.cfg.TTFT.Median {
		t.Error("apex should be faster to first token than bargain")
	}
}
