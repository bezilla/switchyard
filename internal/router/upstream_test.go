package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/provider"
)

// The claim this file exists to check: the routing, breaker and failure-
// taxonomy behavior demonstrated on simulated providers is not a property of
// the simulation. A real provider that stops working has to trip its circuit,
// shed its traffic and hand it to the next candidate in exactly the same
// sequence -- otherwise the real-provider path is decoration on a demo.
//
// The test does not assert that separately for each kind of provider. It runs
// the identical scenario twice, once against a simulated 503 and once against a
// live HTTP server that accepts the connection and then never speaks, and
// requires the two observed sequences to be equal.

// alwaysServes is the fallback: a simulated provider with no error rate, no
// limit and negligible latency, so the only interesting thing in the test is
// what happens to the provider being broken.
func alwaysServes(name string) *provider.Sim {
	return provider.New(provider.Config{
		Name:           name,
		TTFT:           provider.Latency{Floor: time.Millisecond},
		PerToken:       provider.Latency{Floor: time.Millisecond},
		MinReplyTokens: 1,
		MaxReplyTokens: 1,
	})
}

// stalledUpstream is a real HTTP provider in the worst realistic shape: it
// answers 200, flushes headers, and then produces no token at all. This is how
// a saturated inference server actually degrades, and it is invisible to
// anything that only checks whether the connection was accepted.
func stalledUpstream(t *testing.T) *provider.Upstream {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			// Note that the health probe still passes. A stalled server is up.
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	up, err := provider.NewUpstream(provider.UpstreamConfig{
		Name:         "broken",
		BaseURL:      srv.URL + "/v1",
		Model:        "m",
		StartTimeout: provider.Duration(40 * time.Millisecond),
		IdleTimeout:  provider.Duration(40 * time.Millisecond),
		ProbeTimeout: provider.Duration(time.Second),
	}, "")
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	return up
}

// brokenSim is the same shape of outage, simulated.
func brokenSim() *provider.Sim {
	s := provider.New(provider.Config{
		Name:           "broken",
		TTFT:           provider.Latency{Floor: time.Millisecond},
		PerToken:       provider.Latency{Floor: time.Millisecond},
		MinReplyTokens: 1,
		MaxReplyTokens: 1,
	})
	s.Inject(provider.Injection{Mode: provider.ModeError, Rate: 1})
	return s
}

// run drives requests through a router whose primary is broken and whose
// secondary always serves, and returns what was observed: the provider that
// served each request, and the outcome recorded against the broken primary.
func run(t *testing.T, broken provider.Provider, requests int) (served []string, primary []Outcome, endState string) {
	t.Helper()

	cfg := breaker.DefaultConfig()
	primaryBreaker := breaker.New(cfg)
	rt := New(PolicyFailover, nil,
		&Target{Provider: broken, Priority: 10, Breaker: primaryBreaker},
		&Target{Provider: alwaysServes("fallback"), Priority: 20, Breaker: breaker.New(cfg)},
	)

	for i := 0; i < requests; i++ {
		stream, d, err := rt.Route(context.Background(), provider.Request{Prompt: "hi", MaxTokens: 1})
		if err != nil {
			t.Fatalf("request %d: every provider refused: %v", i, err)
		}
		_, _, _ = provider.Drain(context.Background(), stream)

		served = append(served, d.Provider)
		for _, a := range d.Attempts {
			if a.Provider == "broken" {
				primary = append(primary, a.Outcome)
			}
		}
	}
	return served, primary, primaryBreaker.State().String()
}

func TestRealProviderTripsTheBreakerExactlyAsASimulatedOneDoes(t *testing.T) {
	const requests = 14

	simServed, simOutcomes, simState := run(t, brokenSim(), requests)
	realServed, realOutcomes, realState := run(t, stalledUpstream(t), requests)

	// 1. Every request was served, by the fallback, in both worlds. The caller
	//    never sees the outage either way.
	for i := range simServed {
		if simServed[i] != "fallback" || realServed[i] != "fallback" {
			t.Fatalf("request %d served by sim=%q real=%q, want fallback for both",
				i, simServed[i], realServed[i])
		}
	}

	// 2. The sequence of outcomes recorded against the broken provider is the
	//    same: refused while the circuit is closed, then breaker_open once it
	//    trips and the router stops asking.
	if len(simOutcomes) != len(realOutcomes) {
		t.Fatalf("attempt counts differ: sim %d, real %d", len(simOutcomes), len(realOutcomes))
	}
	for i := range simOutcomes {
		if simOutcomes[i] != realOutcomes[i] {
			t.Fatalf("attempt %d: sim outcome %q, real outcome %q -- the real provider is not being treated like the simulated one",
				i, simOutcomes[i], realOutcomes[i])
		}
	}

	// 3. Both circuits ended up open. Stated separately from the comparison
	//    because two identical sequences of "refused" would also match.
	if simState != "open" || realState != "open" {
		t.Fatalf("breaker states: sim %q, real %q, want both open", simState, realState)
	}

	// 4. And the trip actually happened partway through rather than on the
	//    last request, which is what makes the breaker_open entries above
	//    evidence of anything.
	opened := 0
	for _, o := range realOutcomes {
		if o == OutcomeBreakerOpen {
			opened++
		}
	}
	if opened == 0 {
		t.Fatal("the real provider was asked on every request; its circuit never stopped the traffic")
	}
}

// A real 429 must fail over without opening a circuit, for the same reason a
// simulated one does: a provider shedding load is working correctly, and a
// breaker that opened on it would keep traffic away, stop the 429s, and leave
// nothing to ever say it was safe to come back.
func TestRealRateLimitFailsOverWithoutOpeningTheCircuit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m"}]}`)
			return
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit exceeded"}}`)
	}))
	defer srv.Close()

	up, err := provider.NewUpstream(provider.UpstreamConfig{
		Name: "limited", BaseURL: srv.URL + "/v1", Model: "m",
		StartTimeout: provider.Duration(time.Second),
	}, "")
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	cfg := breaker.DefaultConfig()
	br := breaker.New(cfg)
	rt := New(PolicyFailover, nil,
		&Target{Provider: up, Priority: 10, Breaker: br},
		&Target{Provider: alwaysServes("fallback"), Priority: 20, Breaker: breaker.New(cfg)},
	)

	for i := 0; i < 20; i++ {
		stream, d, err := rt.Route(context.Background(), provider.Request{Prompt: "hi", MaxTokens: 1})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _, _ = provider.Drain(context.Background(), stream)
		if d.Provider != "fallback" || d.Failovers != 1 {
			t.Fatalf("request %d: provider %q with %d failover(s), want fallback after one",
				i, d.Provider, d.Failovers)
		}
	}

	if got := br.State().String(); got != "closed" {
		t.Errorf("breaker is %q after twenty real 429s, want closed: a rate limit is not ill health", got)
	}
}
