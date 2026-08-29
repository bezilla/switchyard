package provider

import (
	"context"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// EstimatePromptTokens approximates a tokenizer at roughly four characters per
// token. Real tokenizers disagree with this and with each other; the number
// only has to be stable and roughly right for the cost panels to mean anything.
func EstimatePromptTokens(prompt string) int {
	n := (len(prompt) + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}

// Latency describes a simulated latency distribution as a lognormal, which is
// the shape real request latency actually has: a firm floor, a dense body, and
// a tail that runs well past the median. Median and Sigma are the lognormal
// parameters; Floor is added afterwards as irreducible overhead.
type Latency struct {
	Median time.Duration
	Sigma  float64
	Floor  time.Duration
}

// Sample draws one latency.
func (l Latency) Sample(rng *rand.Rand) time.Duration {
	if l.Median <= 0 {
		return l.Floor
	}
	d := time.Duration(float64(l.Median) * math.Exp(l.Sigma*rng.NormFloat64()))
	return l.Floor + d
}

// RateLimit is a token bucket, the shape of limit real providers actually
// enforce: a sustained rate with a burst allowance on top.
type RateLimit struct {
	// RequestsPerSecond is the sustained refill rate.
	RequestsPerSecond float64

	// Burst is the bucket size, so a quiet period buys a short spike.
	Burst float64
}

// Config describes one simulated provider's personality.
type Config struct {
	Name  string
	Rates Rates

	// TTFT is time to first token: how long the provider thinks before it
	// starts talking.
	TTFT Latency

	// PerToken is the gap between streamed chunks once talking.
	PerToken Latency

	// ErrorRate is the baseline probability that a request fails outright,
	// before any injected failure.
	ErrorRate float64

	// Limit, when set, rate-limits the provider with 429-equivalents.
	Limit *RateLimit

	// MaxConcurrent caps simultaneous in-flight streams. Zero means no cap.
	// This models a fixed-size box rather than an elastic service.
	MaxConcurrent int

	// TokensPerReply bounds the simulated completion length.
	MinReplyTokens int
	MaxReplyTokens int

	// Seed makes the whole provider reproducible run to run.
	Seed uint64
}

// Mode is an injected failure state, set through the admin endpoint.
type Mode string

const (
	// ModeHealthy is the provider behaving as configured.
	ModeHealthy Mode = "healthy"

	// ModeError makes the provider return unavailable errors.
	ModeError Mode = "error"

	// ModeRateLimit makes the provider return 429-equivalents.
	ModeRateLimit Mode = "ratelimit"

	// ModeSlow multiplies latency without failing, which is the failure mode
	// that hurts most and shows up least in an up/down health check.
	ModeSlow Mode = "slow"
)

// Injection is the currently injected fault, if any.
type Injection struct {
	Mode Mode `json:"mode"`

	// Rate is the fraction of requests the mode applies to, 0..1. A partial
	// rate is the realistic case: providers usually degrade rather than stop.
	Rate float64 `json:"rate"`

	// SlowFactor multiplies latency in ModeSlow.
	SlowFactor float64 `json:"slow_factor,omitempty"`
}

// Sim is a simulated inference provider. It performs no network I/O and holds
// no credentials: everything it does is arithmetic and sleeping.
type Sim struct {
	cfg Config

	mu        sync.Mutex
	inject    Injection
	bucket    float64
	lastFill  time.Time
	inflight  int
	requestNo uint64
}

var _ Provider = (*Sim)(nil)

// New builds a simulated provider.
func New(cfg Config) *Sim {
	if cfg.MinReplyTokens <= 0 {
		cfg.MinReplyTokens = 40
	}
	if cfg.MaxReplyTokens < cfg.MinReplyTokens {
		cfg.MaxReplyTokens = cfg.MinReplyTokens + 200
	}
	s := &Sim{
		cfg:      cfg,
		inject:   Injection{Mode: ModeHealthy},
		lastFill: time.Now(),
	}
	if cfg.Limit != nil {
		s.bucket = cfg.Limit.Burst
	}
	return s
}

// Name implements Provider.
func (s *Sim) Name() string { return s.cfg.Name }

// Rates implements Provider.
func (s *Sim) Rates() Rates { return s.cfg.Rates }

// Inject sets the injected failure mode. Passing ModeHealthy clears it.
func (s *Sim) Inject(in Injection) {
	if in.Mode == ModeSlow && in.SlowFactor <= 0 {
		in.SlowFactor = 8
	}
	if in.Mode != ModeHealthy && in.Rate <= 0 {
		in.Rate = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inject = in
}

// Injection reports the current injected failure mode.
func (s *Sim) Injection() Injection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inject
}

// Inflight reports how many streams are open right now.
func (s *Sim) Inflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// Capacity reports the configured concurrency cap, or 0 for uncapped.
func (s *Sim) Capacity() int { return s.cfg.MaxConcurrent }

// rngFor derives a deterministic random source for a request. Seeding from the
// request content means the same prompt draws the same latency and the same
// reply on the same provider, every run: a demo that shifts under you teaches
// nothing. State that genuinely depends on timing -- the rate-limit bucket and
// the concurrency count -- is deliberately outside this.
func (s *Sim) rngFor(req Request) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s.cfg.Name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.Model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.Prompt))
	return rand.New(rand.NewSource(int64(h.Sum64() ^ s.cfg.Seed))) //nolint:gosec // simulation, not security
}

// Probe implements Provider. It is the health check: it reports what a caller
// would find if it tried, without consuming rate limit or capacity. An injected
// error mode shows up here, which is how the health checker learns about a
// provider that nobody happens to be calling.
func (s *Sim) Probe(ctx context.Context) error {
	s.mu.Lock()
	in := s.inject
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return &Failure{Provider: s.cfg.Name, Kind: KindTimeout, Message: "probe canceled"}
	default:
	}

	switch in.Mode {
	case ModeError:
		// A partially failing provider fails a proportional share of probes,
		// so health is a signal with noise in it rather than a clean switch.
		if in.Rate >= 1 || rand.Float64() < in.Rate { //nolint:gosec // simulation, not security
			return &Failure{
				Provider: s.cfg.Name,
				Kind:     KindUnavailable,
				Message:  "upstream returned 503 to health probe",
			}
		}
	case ModeRateLimit:
		return &Failure{
			Provider:   s.cfg.Name,
			Kind:       KindRateLimited,
			Message:    "health probe rate limited",
			RetryAfter: 2 * time.Second,
		}
	case ModeHealthy, ModeSlow:
		// A slow provider is still up, and saying otherwise would hide exactly
		// the failure mode that is hardest to see.
	}
	return nil
}

// Start implements Provider. Every way this provider can refuse work is
// surfaced here, before a Stream exists, so that the router can reroute without
// the caller observing anything.
func (s *Sim) Start(ctx context.Context, req Request) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, &Failure{Provider: s.cfg.Name, Kind: KindTimeout, Message: "canceled before start"}
	}

	rng := s.rngFor(req)

	s.mu.Lock()
	in := s.inject
	s.requestNo++
	// A per-request nudge so injected partial failures do not land on the same
	// requests every time, the way a real flaky upstream does not.
	jitter := rand.New(rand.NewSource(int64(s.requestNo))) //nolint:gosec // simulation, not security
	s.mu.Unlock()

	// --- injected faults, checked before real limits --------------------------
	switch in.Mode {
	case ModeError:
		if in.Rate >= 1 || jitter.Float64() < in.Rate {
			return nil, &Failure{
				Provider: s.cfg.Name,
				Kind:     KindUnavailable,
				Message:  "upstream returned 503",
			}
		}
	case ModeRateLimit:
		if in.Rate >= 1 || jitter.Float64() < in.Rate {
			return nil, &Failure{
				Provider:   s.cfg.Name,
				Kind:       KindRateLimited,
				Message:    "quota exceeded",
				RetryAfter: 3 * time.Second,
			}
		}
	case ModeHealthy, ModeSlow:
	}

	// --- baseline error rate --------------------------------------------------
	if s.cfg.ErrorRate > 0 && jitter.Float64() < s.cfg.ErrorRate {
		return nil, &Failure{
			Provider: s.cfg.Name,
			Kind:     KindUnavailable,
			Message:  "upstream returned 500",
		}
	}

	// --- rate limit -----------------------------------------------------------
	if err := s.takeToken(); err != nil {
		return nil, err
	}

	// --- concurrency capacity -------------------------------------------------
	if err := s.acquireSlot(); err != nil {
		return nil, err
	}

	slow := 1.0
	if in.Mode == ModeSlow {
		slow = in.SlowFactor
	}

	promptTokens := EstimatePromptTokens(req.Prompt)
	reply := s.cfg.MinReplyTokens + rng.Intn(s.cfg.MaxReplyTokens-s.cfg.MinReplyTokens+1)
	if req.MaxTokens > 0 && reply > req.MaxTokens {
		reply = req.MaxTokens
	}

	return &simStream{
		sim:          s,
		rng:          rng,
		slow:         slow,
		ttft:         time.Duration(float64(s.cfg.TTFT.Sample(rng)) * slow),
		perToken:     s.cfg.PerToken,
		total:        reply,
		promptTokens: promptTokens,
	}, nil
}

// takeToken applies the rate limit, refilling the bucket for elapsed time.
func (s *Sim) takeToken() error {
	if s.cfg.Limit == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(s.lastFill).Seconds()
	s.lastFill = now
	s.bucket = math.Min(s.cfg.Limit.Burst, s.bucket+elapsed*s.cfg.Limit.RequestsPerSecond)

	if s.bucket < 1 {
		// How long until one whole token exists again.
		wait := time.Duration((1 - s.bucket) / s.cfg.Limit.RequestsPerSecond * float64(time.Second))
		return &Failure{
			Provider:   s.cfg.Name,
			Kind:       KindRateLimited,
			Message:    "rate limit exceeded",
			RetryAfter: wait,
		}
	}
	s.bucket--
	return nil
}

// acquireSlot applies the concurrency cap. It refuses rather than queues: a
// queue in front of a full box converts a capacity problem into a latency
// problem, and the caller usually prefers the honest refusal.
func (s *Sim) acquireSlot() error {
	if s.cfg.MaxConcurrent <= 0 {
		s.mu.Lock()
		s.inflight++
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight >= s.cfg.MaxConcurrent {
		return &Failure{
			Provider: s.cfg.Name,
			Kind:     KindCapacity,
			Message:  "no capacity: all slots busy",
		}
	}
	s.inflight++
	return nil
}

func (s *Sim) releaseSlot() {
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.mu.Unlock()
}

// simStream is one in-flight simulated completion.
type simStream struct {
	sim      *Sim
	rng      *rand.Rand
	slow     float64
	ttft     time.Duration
	perToken Latency

	total        int
	emitted      int
	promptTokens int

	started bool
	once    sync.Once
	closed  bool
	mu      sync.Mutex
}

// lexicon is a small closed vocabulary. The point is that a completion looks
// like prose in a terminal, not that it means anything.
var lexicon = strings.Fields(`the request arrives at an edge that must decide
quickly which upstream can serve it now rather than which one served it best
last week routing is a claim about the present and health is how that claim
stays true when a provider stops answering the interesting question is not
whether failover happens but what it costs and whether anyone can see it`)

func (t *simStream) Next(ctx context.Context) (Chunk, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return Chunk{}, ErrStreamClosed
	}
	first := !t.started
	t.started = true
	t.mu.Unlock()

	delay := t.ttft
	if !first {
		delay = time.Duration(float64(t.perToken.Sample(t.rng)) * t.slow)
	}

	select {
	case <-ctx.Done():
		return Chunk{}, &Failure{
			Provider: t.sim.Name(),
			Kind:     KindTimeout,
			Message:  "context canceled mid-stream",
		}
	case <-time.After(delay):
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return Chunk{}, ErrStreamClosed
	}
	if t.emitted >= t.total {
		return Chunk{}, io.EOF
	}

	word := lexicon[t.rng.Intn(len(lexicon))]
	idx := t.emitted
	t.emitted++
	return Chunk{Text: word + " ", Index: idx}, nil
}

func (t *simStream) Usage() Usage {
	t.mu.Lock()
	completion := t.emitted
	t.mu.Unlock()
	return Usage{
		PromptTokens:     t.promptTokens,
		CompletionTokens: completion,
		CostUSD:          t.sim.Rates().Cost(t.promptTokens, completion),
	}
}

func (t *simStream) Close() error {
	t.once.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.sim.releaseSlot()
	})
	return nil
}
