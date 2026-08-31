package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/provider"
	"github.com/bezilla/switchyard/internal/router"
	"github.com/bezilla/switchyard/internal/telemetry"
)

// fakeProvider is a provider whose refusal the test dictates. The simulated
// providers are deliberately not used: they carry latency distributions and a
// baseline error rate, and a test about header ordering that depends on either
// is a test that flakes.
//
// starts is shared across every provider in a case so the test can ask "how
// many Start calls had happened by the time the header went out", which is the
// only way to observe the ordering from outside the handler.
type fakeProvider struct {
	name   string
	kind   provider.FailureKind // empty means it serves
	starts *int
}

func (f *fakeProvider) Name() string                { return f.name }
func (f *fakeProvider) Rates() provider.Rates       { return provider.Rates{} }
func (f *fakeProvider) Probe(context.Context) error { return nil }

func (f *fakeProvider) Start(context.Context, provider.Request) (provider.Stream, error) {
	*f.starts++
	if f.kind != "" {
		return nil, &provider.Failure{Provider: f.name, Kind: f.kind, Message: "refused by test"}
	}
	return &fakeStream{chunks: 2}, nil
}

type fakeStream struct {
	chunks  int
	emitted int
}

func (s *fakeStream) Next(context.Context) (provider.Chunk, error) {
	if s.emitted >= s.chunks {
		return provider.Chunk{}, io.EOF
	}
	s.emitted++
	return provider.Chunk{Text: "x", Index: s.emitted - 1}, nil
}

func (s *fakeStream) Usage() provider.Usage { return provider.Usage{CompletionTokens: s.emitted} }
func (s *fakeStream) Close() error          { return nil }

// orderingRecorder answers the question the header-ordering guarantee is about:
// at the instant the status line and headers were committed, how much routing
// had already happened?
//
// http.ResponseWriter has no memory of ordering, so the handler could not be
// caught writing early by inspecting the finished response. Snapshotting the
// shared Start counter inside WriteHeader is what makes the ordering visible.
type orderingRecorder struct {
	*httptest.ResponseRecorder
	starts *int

	wrote            bool
	startsAtHeader   int
	providerAtHeader string
}

func (w *orderingRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.startsAtHeader = *w.starts
		w.providerAtHeader = w.Header().Get("X-Switchyard-Provider")
	}
	w.ResponseRecorder.WriteHeader(code)
}

func (w *orderingRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseRecorder.Write(b)
}

// newTestServer builds a gateway over the given providers, in the order given,
// with priority ascending so the first entry is the primary.
func newTestServer(t *testing.T, providers ...provider.Provider) http.Handler {
	t.Helper()

	tel, err := telemetry.New(context.Background(), "test")
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	targets := make([]*router.Target, 0, len(providers))
	for i, p := range providers {
		targets = append(targets, &router.Target{
			Provider: p,
			Priority: (i + 1) * 10,
			Breaker:  breaker.New(breaker.DefaultConfig()),
		})
	}

	r := router.New(router.PolicyFailover, tel.Observer(), targets...)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(r, tel, nil, log, "test").Handler()
}

func chatRequestBody() io.Reader {
	return strings.NewReader(`{"prompt":"hello world","max_tokens":8}`)
}

// lastDoneEvent returns the payload of the terminal done event, so the test can
// check that the body and the headers agree about who served the request.
func lastDoneEvent(t *testing.T, body string) map[string]any {
	t.Helper()
	var payload map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(block, "event: done\n") {
			continue
		}
		data := strings.TrimPrefix(strings.SplitN(block, "\n", 2)[1], "data: ")
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("decode done event %q: %v", data, err)
		}
	}
	return payload
}

// TestChatHeaderOrdering is the test for the guarantee the whole design rests
// on: the response header naming the provider is written only after some
// provider has accepted the request, and it never names a provider that
// refused. Until the first byte goes out the gateway is free to change its
// mind, and that freedom is what makes failover invisible to the caller.
func TestChatHeaderOrdering(t *testing.T) {
	const serves = provider.FailureKind("")

	tests := []struct {
		name string
		// kinds, in priority order. An empty kind serves.
		kinds []provider.FailureKind

		wantStatus    int
		wantProvider  string
		wantFailovers string
		// wantStarts is how many Start calls must have completed before the
		// header was committed: every refusal, plus the provider that served.
		wantStarts int
	}{
		{
			name:          "primary serves, nobody else is asked",
			kinds:         []provider.FailureKind{serves, serves},
			wantStatus:    http.StatusOK,
			wantProvider:  "p0",
			wantFailovers: "0",
			wantStarts:    1,
		},
		{
			name:          "primary refuses, header names the one that accepted",
			kinds:         []provider.FailureKind{provider.KindUnavailable, serves},
			wantStatus:    http.StatusOK,
			wantProvider:  "p1",
			wantFailovers: "1",
			wantStarts:    2,
		},
		{
			name:          "two refuse, header names the third",
			kinds:         []provider.FailureKind{provider.KindUnavailable, provider.KindRateLimited, serves},
			wantStatus:    http.StatusOK,
			wantProvider:  "p2",
			wantFailovers: "2",
			wantStarts:    3,
		},
		{
			name:          "a rate limit is a refusal like any other to the caller",
			kinds:         []provider.FailureKind{provider.KindRateLimited, serves},
			wantStatus:    http.StatusOK,
			wantProvider:  "p1",
			wantFailovers: "1",
			wantStarts:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			starts := 0
			providers := make([]provider.Provider, 0, len(tt.kinds))
			refusers := make([]string, 0, len(tt.kinds))
			for i, k := range tt.kinds {
				name := "p" + string(rune('0'+i))
				providers = append(providers, &fakeProvider{name: name, kind: k, starts: &starts})
				if k != serves {
					refusers = append(refusers, name)
				}
			}

			h := newTestServer(t, providers...)
			rec := &orderingRecorder{ResponseRecorder: httptest.NewRecorder(), starts: &starts}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat", chatRequestBody())
			req.Header.Set("content-type", "application/json")

			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("X-Switchyard-Provider"); got != tt.wantProvider {
				t.Fatalf("X-Switchyard-Provider = %q, want %q", got, tt.wantProvider)
			}
			if got := rec.Header().Get("X-Switchyard-Failovers"); got != tt.wantFailovers {
				t.Fatalf("X-Switchyard-Failovers = %q, want %q", got, tt.wantFailovers)
			}

			// The ordering assertion. If the handler had committed the header
			// before routing finished, fewer Start calls would have completed
			// by then -- and on the failover cases the header would name a
			// provider that went on to refuse.
			if rec.startsAtHeader != tt.wantStarts {
				t.Fatalf("%d provider Start call(s) had completed when the header was "+
					"written, want %d: the header must not be committed until a "+
					"provider has accepted", rec.startsAtHeader, tt.wantStarts)
			}
			if rec.providerAtHeader != tt.wantProvider {
				t.Fatalf("header named %q at the moment it was committed, want %q",
					rec.providerAtHeader, tt.wantProvider)
			}
			for _, bad := range refusers {
				if rec.providerAtHeader == bad {
					t.Fatalf("header named %q, a provider that refused this request", bad)
				}
			}

			// Body and headers must agree. A caller that reads one and a
			// dashboard that reads the other should not disagree about who
			// served the request.
			done := lastDoneEvent(t, rec.Body.String())
			if done == nil {
				t.Fatal("no done event in the stream")
			}
			if got, _ := done["provider"].(string); got != tt.wantProvider {
				t.Fatalf("done event provider = %q, want %q", got, tt.wantProvider)
			}
		})
	}
}

// TestChatNoProviderAvailable covers the path where nothing will serve. The
// header must not name a provider, because naming one that never accepted
// would send a reader chasing a provider that did nothing wrong.
func TestChatNoProviderAvailable(t *testing.T) {
	tests := []struct {
		name  string
		kinds []provider.FailureKind
	}{
		{
			name:  "every provider is down",
			kinds: []provider.FailureKind{provider.KindUnavailable, provider.KindUnavailable},
		},
		{
			name:  "every provider is rate limited",
			kinds: []provider.FailureKind{provider.KindRateLimited, provider.KindRateLimited},
		},
		{
			name:  "a mix of refusal kinds",
			kinds: []provider.FailureKind{provider.KindUnavailable, provider.KindRateLimited, provider.KindCapacity},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			starts := 0
			providers := make([]provider.Provider, 0, len(tt.kinds))
			for i, k := range tt.kinds {
				providers = append(providers,
					&fakeProvider{name: "p" + string(rune('0'+i)), kind: k, starts: &starts})
			}

			h := newTestServer(t, providers...)
			rec := &orderingRecorder{ResponseRecorder: httptest.NewRecorder(), starts: &starts}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat", chatRequestBody())
			req.Header.Set("content-type", "application/json")

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
			}
			if got := rec.Header().Get("X-Switchyard-Provider"); got != "" {
				t.Fatalf("X-Switchyard-Provider = %q, want it absent: no provider "+
					"served this request", got)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json: the failure is a "+
					"JSON document, not a stream", got)
			}

			var body struct {
				Error    string `json:"error"`
				Attempts []any  `json:"attempts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if body.Error != "no provider available" {
				t.Fatalf("error = %q, want %q", body.Error, "no provider available")
			}
			if len(body.Attempts) != len(tt.kinds) {
				t.Fatalf("attempts = %d, want %d: every candidate should be reported "+
					"even when none served", len(body.Attempts), len(tt.kinds))
			}
			if starts != len(tt.kinds) {
				t.Fatalf("Start was called %d time(s), want %d: every provider should "+
					"have been asked", starts, len(tt.kinds))
			}
		})
	}
}

// TestChatRejectsBadRequests keeps the validation ahead of routing: a request
// that was never going to be served should not consume a provider attempt.
func TestChatRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed json", body: `{"prompt":`},
		{name: "missing prompt", body: `{"max_tokens":8}`},
		{name: "empty prompt", body: `{"prompt":"","max_tokens":8}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			starts := 0
			h := newTestServer(t, &fakeProvider{name: "p0", starts: &starts})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(tt.body))
			req.Header.Set("content-type", "application/json")

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if rec.Header().Get("X-Switchyard-Provider") != "" {
				t.Fatal("a rejected request named a provider")
			}
			if starts != 0 {
				t.Fatalf("Start was called %d time(s) for a request that never "+
					"validated, want 0", starts)
			}
		})
	}
}
