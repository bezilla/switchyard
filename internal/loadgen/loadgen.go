// Package loadgen keeps traffic flowing through the gateway so that the
// dashboards always have something to show.
//
// A demo that requires you to run curl in another window before anything
// happens is a demo where the interesting part -- the moment traffic moves --
// competes for attention with the boring part. Here the traffic is already
// there, and the only thing an operator does is break something.
package loadgen

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bezilla/switchyard/internal/provider"
	"github.com/bezilla/switchyard/internal/router"
	"github.com/bezilla/switchyard/internal/telemetry"
)

// Generator issues synthetic requests through the router at a controllable
// rate. It calls the router in process rather than over HTTP: the HTTP layer is
// not what this project is about, and looping traffic through a socket would
// add a queue whose behavior under load would confuse the failover story with
// the load generator's own.
type Generator struct {
	router *router.Router
	tel    *telemetry.Telemetry
	log    *slog.Logger

	// rate is requests per second, read and written atomically because the
	// admin endpoint changes it while the generator is running.
	rate atomic.Uint64 // float64 bits

	// maxInflight bounds concurrency so that a spike, or a stall on every
	// provider at once, cannot grow goroutines without limit. Requests over
	// the bound are recorded as shed rather than queued: a load generator that
	// silently queues stops measuring what you asked it to measure.
	maxInflight int
	inflight    atomic.Int64

	prompts []string
	rng     *rand.Rand
	rngMu   sync.Mutex
}

// Config tunes the generator.
type Config struct {
	// RPS is the initial offered load.
	RPS float64

	// MaxInflight bounds concurrent requests.
	MaxInflight int

	// Seed makes prompt selection reproducible.
	Seed uint64
}

// New builds a generator.
func New(r *router.Router, tel *telemetry.Telemetry, log *slog.Logger, cfg Config) *Generator {
	if cfg.RPS <= 0 {
		cfg.RPS = 12
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 256
	}
	g := &Generator{
		router:      r,
		tel:         tel,
		log:         log,
		maxInflight: cfg.MaxInflight,
		prompts:     prompts,
		// The seed is a uint64 everywhere else because it is an opaque label
		// rather than a quantity; reinterpreting its bits here is the whole
		// conversion, and no value is lost.
		rng: rand.New(rand.NewSource(int64(cfg.Seed))), //nolint:gosec // synthetic load, not security
	}
	g.SetRate(cfg.RPS)
	return g
}

// SetRate changes the offered load. Implements server.Controller.
func (g *Generator) SetRate(rps float64) {
	if rps < 0 {
		rps = 0
	}
	g.rate.Store(mathFloatBits(rps))
}

// Rate reports the offered load. Implements server.Controller.
func (g *Generator) Rate() float64 { return mathFloatFromBits(g.rate.Load()) }

// Inflight reports how many synthetic requests are open.
func (g *Generator) Inflight() int64 { return g.inflight.Load() }

// Run issues traffic until the context is canceled.
//
// Arrivals are exponentially distributed rather than evenly spaced. Evenly
// spaced arrivals are the one traffic pattern that never happens and the one
// that makes rate limits and capacity caps look far kinder than they are: a
// token bucket sized for the mean will pass a metronome and refuse a Poisson
// stream at the same mean.
func (g *Generator) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		rps := g.Rate()
		if rps <= 0 {
			// Paused. Wake up periodically in case the rate changes.
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}

		wait := g.nextArrival(rps)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		if g.inflight.Load() >= int64(g.maxInflight) {
			g.tel.RecordShed(ctx)
			continue
		}

		g.inflight.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer g.inflight.Add(-1)
			g.one(ctx)
		}()
	}
}

// nextArrival draws an exponential inter-arrival gap for the given rate.
func (g *Generator) nextArrival(rps float64) time.Duration {
	g.rngMu.Lock()
	u := g.rng.Float64()
	g.rngMu.Unlock()
	if u <= 0 {
		u = 1e-9
	}
	gap := -mathLog(u) / rps
	// Cap the tail so a very unlucky draw at a low rate does not stall the
	// generator for minutes.
	if gap > 2 {
		gap = 2
	}
	return time.Duration(gap * float64(time.Second))
}

// one issues a single request and records it, exactly as the HTTP handler
// would. The generator draining the stream rather than discarding it matters:
// the token and cost metrics are only true if somebody reads the tokens.
func (g *Generator) one(ctx context.Context) {
	g.rngMu.Lock()
	prompt := g.prompts[g.rng.Intn(len(g.prompts))]
	maxTokens := 64 + g.rng.Intn(192)
	g.rngMu.Unlock()

	// A per-request deadline, because a request that never ends is
	// indistinguishable from a provider that never answers, and only one of
	// those should hold a slot forever.
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	started := time.Now()
	stream, decision, err := g.router.Route(rctx, provider.Request{
		Model:     "default",
		Prompt:    prompt,
		MaxTokens: maxTokens,
		Key:       "loadgen",
	})
	if err != nil {
		g.tel.RecordRequest(ctx, decision, telemetry.OutcomeNoProvider, time.Since(started), provider.Usage{}, 0)
		return
	}
	defer func() { _ = stream.Close() }()

	var ttft time.Duration
	for {
		_, err := stream.Next(rctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			outcome := telemetry.OutcomeStreamError
			if errors.Is(ctx.Err(), context.Canceled) {
				outcome = telemetry.OutcomeCanceled
			}
			g.tel.RecordRequest(ctx, decision, outcome, time.Since(started), stream.Usage(), ttft)
			return
		}
		if ttft == 0 {
			ttft = time.Since(started)
		}
	}

	g.tel.RecordRequest(ctx, decision, telemetry.OutcomeSuccess, time.Since(started), stream.Usage(), ttft)
}

// prompts are a fixed set so that prompt-token counts stay in a realistic band
// and the cost panel is not dominated by one enormous outlier.
var prompts = []string{
	"Summarize the tradeoffs between routing on latency and routing on price.",
	"Explain why a circuit breaker that closes all at once causes a second outage.",
	"What does an error budget actually buy an on-call engineer?",
	"Describe the difference between shedding load and being unhealthy.",
	"Draft a short incident note for a provider outage with clean failover.",
	"Why is time to first token a better latency signal than total duration for streaming?",
	"Compare a token bucket to a fixed window for rate limiting a bursty client.",
	"When is queueing in front of a full server worse than refusing the request?",
	"Outline the metrics you would need to prove failover worked, not just happened.",
	"What changes about capacity planning when the expensive dependency is per token?",
}
