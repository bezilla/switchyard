package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sseChunks renders a completion the way an OpenAI-compatible server does, so
// the tests exercise the real parser rather than a convenient shortcut.
func sseChunks(words []string, usage string) string {
	var b strings.Builder
	for _, w := range words {
		fmt.Fprintf(&b, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", w)
	}
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	if usage != "" {
		fmt.Fprintf(&b, "data: {\"choices\":[],\"usage\":%s}\n\n", usage)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func testUpstream(t *testing.T, srv *httptest.Server, mutate func(*UpstreamConfig)) *Upstream {
	t.Helper()
	cfg := UpstreamConfig{
		Name:         "up",
		BaseURL:      srv.URL + "/v1",
		Model:        "test-model",
		StartTimeout: Duration(2 * time.Second),
		IdleTimeout:  Duration(2 * time.Second),
		ProbeTimeout: Duration(time.Second),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	up, err := NewUpstream(cfg, "")
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	return up
}

func TestUpstreamStreamsAndReportsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseChunks([]string{"one ", "two ", "three"},
			`{"prompt_tokens":11,"completion_tokens":3}`))
	}))
	defer srv.Close()

	up := testUpstream(t, srv, nil)
	stream, err := up.Start(context.Background(), Request{Prompt: "hello", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	text, usage, err := Drain(context.Background(), stream)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if text != "one two three" {
		t.Errorf("text = %q, want %q", text, "one two three")
	}
	// The upstream's own counts win over the estimate: it has a tokenizer and
	// this adapter does not.
	if usage.PromptTokens != 11 || usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v, want prompt 11 completion 3", usage)
	}
	if got := up.Inflight(); got != 0 {
		t.Errorf("inflight after close = %d, want 0", got)
	}
}

func TestUpstreamEstimatesUsageWhenUpstreamReportsNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sseChunks([]string{"a ", "b "}, ""))
	}))
	defer srv.Close()

	up := testUpstream(t, srv, func(c *UpstreamConfig) {
		c.Rates = Rates{PromptUSDPerMTok: 1000, CompletionUSDPerMTok: 2000}
	})
	stream, err := up.Start(context.Background(), Request{Prompt: "12345678", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, usage, err := Drain(context.Background(), stream)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if usage.PromptTokens != EstimatePromptTokens("12345678") {
		t.Errorf("prompt tokens = %d, want the estimate %d", usage.PromptTokens, EstimatePromptTokens("12345678"))
	}
	if usage.CompletionTokens != 2 {
		t.Errorf("completion tokens = %d, want 2 (one per emitted chunk)", usage.CompletionTokens)
	}
	if usage.CostUSD <= 0 {
		t.Errorf("cost = %v, want the configured rates applied", usage.CostUSD)
	}
}

// The taxonomy is the whole reason a real provider can be dropped into this
// router without changing anything downstream. If a real 429 did not classify
// as a rate limit, it would open a circuit against a provider that is working.
func TestUpstreamStatusMapsOntoTheFailureTaxonomy(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		retryAfter  string
		body        string
		wantKind    FailureKind
		wantHealth  bool
		wantRetryAt time.Duration
	}{
		{"rate limited", http.StatusTooManyRequests, "3", `{"error":{"message":"slow down"}}`, KindRateLimited, false, 3 * time.Second},
		{"server error", http.StatusInternalServerError, "", "boom", KindUnavailable, true, 0},
		{"service unavailable", http.StatusServiceUnavailable, "", "", KindUnavailable, true, 0},
		{"model missing", http.StatusNotFound, "", `{"error":{"message":"model not found"}}`, KindUnavailable, true, 0},
		{"unauthorized", http.StatusUnauthorized, "", "", KindUnavailable, true, 0},
		{"gateway timeout", http.StatusGatewayTimeout, "", "", KindTimeout, true, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			up := testUpstream(t, srv, nil)
			_, err := up.Start(context.Background(), Request{Prompt: "hi", MaxTokens: 4})
			if err == nil {
				t.Fatal("Start succeeded against an error status")
			}
			var f *Failure
			if !errors.As(err, &f) {
				t.Fatalf("error is %T, want *Failure", err)
			}
			if f.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", f.Kind, tc.wantKind)
			}
			if f.Kind.CountsAgainstHealth() != tc.wantHealth {
				t.Errorf("CountsAgainstHealth = %v, want %v", f.Kind.CountsAgainstHealth(), tc.wantHealth)
			}
			if f.RetryAfter != tc.wantRetryAt {
				t.Errorf("RetryAfter = %v, want %v", f.RetryAfter, tc.wantRetryAt)
			}
			if f.Provider != "up" {
				t.Errorf("provider = %q, want %q", f.Provider, "up")
			}
			// A refused request must not leak a slot, or the cap turns into a
			// slow leak that looks like a capacity problem days later.
			if got := up.Inflight(); got != 0 {
				t.Errorf("inflight after refusal = %d, want 0", got)
			}
		})
	}
}

// The failure mode that matters most for real inference: 200 OK, headers out,
// and then nothing. It has to surface from Start as a timeout -- reroutable,
// and counting against health -- rather than as a stream that hangs.
func TestUpstreamStallBeforeFirstTokenIsAReroutableTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()
	defer close(release)

	up := testUpstream(t, srv, func(c *UpstreamConfig) {
		c.StartTimeout = Duration(150 * time.Millisecond)
	})

	started := time.Now()
	_, err := up.Start(context.Background(), Request{Prompt: "hi", MaxTokens: 4})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("Start returned a stream from an upstream that never sent a token")
	}
	if KindOf(err) != KindTimeout {
		t.Errorf("kind = %q, want %q", KindOf(err), KindTimeout)
	}
	if !KindOf(err).CountsAgainstHealth() {
		t.Error("a start timeout must count against health, or a hung provider is never routed away from")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Start took %v; the start budget did not bound it", elapsed)
	}
	if got := up.Inflight(); got != 0 {
		t.Errorf("inflight after timeout = %d, want 0", got)
	}
}

func TestUpstreamCapacityRefusalCostsNoRoundTrip(t *testing.T) {
	var calls atomic.Int64
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block
	}))
	defer srv.Close()
	defer close(block)

	up := testUpstream(t, srv, func(c *UpstreamConfig) { c.MaxConcurrent = 1 })

	first, err := up.Start(context.Background(), Request{Prompt: "hi", MaxTokens: 8})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = first.Close() }()

	before := calls.Load()
	_, err = up.Start(context.Background(), Request{Prompt: "hi", MaxTokens: 8})
	if KindOf(err) != KindCapacity {
		t.Fatalf("kind = %q, want %q", KindOf(err), KindCapacity)
	}
	if KindOf(err).CountsAgainstHealth() {
		t.Error("a capacity refusal must not count against health: a full box is not a broken box")
	}
	if got := calls.Load(); got != before {
		t.Errorf("the refused request reached the upstream (%d calls, was %d); the cap must be checked first", got, before)
	}
}

func TestUpstreamProbeChecksTheModelIsActuallyLoaded(t *testing.T) {
	var models atomic.Value
	models.Store(`{"object":"list","data":[{"id":"other-model"}]}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe hit %q, want /v1/models", r.URL.Path)
		}
		_, _ = io.WriteString(w, models.Load().(string))
	}))
	defer srv.Close()

	up := testUpstream(t, srv, nil)

	// A server that is up but has never been given these weights answers a
	// liveness check perfectly and 404s every request. The probe has to see
	// the difference, or it is not measuring what requests measure.
	err := up.Probe(context.Background())
	if KindOf(err) != KindUnavailable || err == nil {
		t.Fatalf("probe on a server missing the model returned %v, want an unavailable failure", err)
	}

	models.Store(`{"object":"list","data":[{"id":"test-model"}]}`)
	if err := up.Probe(context.Background()); err != nil {
		t.Fatalf("probe with the model loaded: %v", err)
	}
}

func TestUpstreamProbeClassifiesAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	up, err := NewUpstream(UpstreamConfig{
		Name: "gone", BaseURL: url + "/v1", Model: "m",
		ProbeTimeout: Duration(500 * time.Millisecond),
	}, "")
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	perr := up.Probe(context.Background())
	if KindOf(perr) != KindUnavailable {
		t.Errorf("kind = %q, want %q", KindOf(perr), KindUnavailable)
	}
	if !KindOf(perr).CountsAgainstHealth() {
		t.Error("an unreachable upstream must count against health")
	}
}

func TestUpstreamIdleStallEndsTheStream(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first \"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	up := testUpstream(t, srv, func(c *UpstreamConfig) {
		c.IdleTimeout = Duration(150 * time.Millisecond)
	})
	stream, err := up.Start(context.Background(), Request{Prompt: "hi", MaxTokens: 64})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	// The second token never comes. Past the first token the request is
	// committed to this provider, so this is a failed request rather than a
	// reroutable one -- but it must still end.
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("Next returned no error against a stalled stream")
	}
}

func TestUpstreamSendsTheTranscriptWhenTheCallerSuppliedOne(t *testing.T) {
	var got struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
		Stream   bool      `json:"stream"`
	}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = decodeJSON(r.Body, &got)
		mu.Unlock()
		_, _ = io.WriteString(w, sseChunks([]string{"ok"}, ""))
	}))
	defer srv.Close()

	up := testUpstream(t, srv, nil)
	stream, err := up.Start(context.Background(), Request{
		Prompt:    "system: be brief\nuser: hi",
		Messages:  []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}},
		MaxTokens: 8,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _, _ = Drain(context.Background(), stream)

	mu.Lock()
	defer mu.Unlock()
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Content != "hi" {
		t.Errorf("messages = %+v, want the caller's two turns intact", got.Messages)
	}
	if got.Model != "test-model" {
		t.Errorf("model = %q, want the upstream's configured model", got.Model)
	}
	if !got.Stream {
		t.Error("the adapter must ask the upstream to stream, or Start cannot see the first token")
	}
}

func TestLoadUpstreams(t *testing.T) {
	t.Run("empty is not an error", func(t *testing.T) {
		cfgs, err := LoadUpstreams("  ")
		if err != nil || cfgs != nil {
			t.Fatalf("got %v, %v; want no upstreams and no error", cfgs, err)
		}
	})

	t.Run("inline json", func(t *testing.T) {
		cfgs, err := LoadUpstreams(`[{"name":"ollama","base_url":"http://x/v1","model":"m","priority":5,"start_timeout":"30s"}]`)
		if err != nil {
			t.Fatalf("LoadUpstreams: %v", err)
		}
		if len(cfgs) != 1 || cfgs[0].Priority != 5 {
			t.Fatalf("cfgs = %+v", cfgs)
		}
		if time.Duration(cfgs[0].StartTimeout) != 30*time.Second {
			t.Errorf("start_timeout = %v, want 30s", time.Duration(cfgs[0].StartTimeout))
		}
	})

	t.Run("a file", func(t *testing.T) {
		path := t.TempDir() + "/up.json"
		if err := writeFile(path, `[{"name":"vllm","base_url":"http://y/v1","model":"m"}]`); err != nil {
			t.Fatal(err)
		}
		cfgs, err := LoadUpstreams("@" + path)
		if err != nil || len(cfgs) != 1 || cfgs[0].Name != "vllm" {
			t.Fatalf("got %+v, %v", cfgs, err)
		}
	})

	t.Run("a misspelled key is an error, not a shrug", func(t *testing.T) {
		_, err := LoadUpstreams(`[{"name":"x","base_url":"http://y/v1","model":"m","max_concurent":2}]`)
		if err == nil {
			t.Fatal("a typo in a config key was accepted silently")
		}
	})

	t.Run("a name collision is refused", func(t *testing.T) {
		cfgs, err := LoadUpstreams(`[{"name":"apex","base_url":"http://y/v1","model":"m"}]`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := BuildUpstreams(cfgs, []string{"apex", "bargain", "local"}); err == nil {
			t.Fatal("an upstream was allowed to take a simulated provider's name")
		}
	})
}

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

func TestLoadUpstreamsIgnoresCommentKeys(t *testing.T) {
	cfgs, err := LoadUpstreams(`[{"//name":"what this is","name":"ollama","base_url":"http://x/v1","model":"m","max_concurrent":2}]`)
	if err != nil {
		t.Fatalf("LoadUpstreams: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].Name != "ollama" || cfgs[0].MaxConcurrent != 2 {
		t.Fatalf("cfgs = %+v", cfgs)
	}
}

// The checked-in template is the file people copy. If it stops parsing, the
// documented opt-in path is broken and nothing else in the repository notices.
func TestShippedOllamaConfigParses(t *testing.T) {
	cfgs, err := LoadUpstreams("@../../deploy/upstreams/ollama.json")
	if err != nil {
		t.Fatalf("the shipped ollama config does not parse: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("got %d upstreams, want 1", len(cfgs))
	}
	c := cfgs[0]
	if c.Name != "ollama" || c.Model == "" || c.BaseURL == "" {
		t.Fatalf("config = %+v", c)
	}
	if c.MaxConcurrent <= 0 {
		t.Error("the shipped config must cap concurrency: an uncapped local model turns one slow request into ten")
	}
	if _, err := BuildUpstreams(cfgs, []string{"apex", "bargain", "local"}); err != nil {
		t.Fatalf("BuildUpstreams: %v", err)
	}
}
