package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upstream is a real inference provider speaking the OpenAI chat-completions
// wire format over HTTP.
//
// It is deliberately not an adapter for any one vendor. Ollama, vLLM, LocalAI,
// llama.cpp's server and every hosted gateway that claims OpenAI compatibility
// expose the same two routes -- POST {base}/chat/completions and GET
// {base}/models -- so one adapter plus a base URL reaches all of them, and
// adding the next one is a config entry rather than a Go file. Ollama is the
// first consumer of this type, not a special case inside it.
//
// The contract it has to honor is the one in Provider.Start: every way this
// upstream can refuse must surface from Start, before a Stream exists, or
// failover stops being invisible to the caller. Two things make that true over
// a network:
//
//   - The concurrency cap is checked before the request is sent, so a full box
//     refuses in microseconds and the router moves on without a round trip.
//   - Start does not return until the first content token has arrived. An
//     upstream that accepts the connection, returns 200, and then thinks for
//     forty seconds is the most common way real inference degrades, and it is
//     indistinguishable from a healthy one until the first token. Buying that
//     distinction costs Start the time-to-first-token it was going to spend
//     anyway; the token is buffered and handed straight back on the first
//     Next, so nothing is spent twice.
type Upstream struct {
	cfg    UpstreamConfig
	client *http.Client
	apiKey string

	mu       sync.Mutex
	inflight int
}

var _ Provider = (*Upstream)(nil)

// Duration is a time.Duration that decodes from a JSON string such as "20s",
// because a config file full of nanosecond integers is a config file nobody
// can read.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"20s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UpstreamConfig describes one OpenAI-compatible endpoint. It is the JSON the
// gateway reads from -upstreams, and it is the whole of what distinguishes one
// upstream from another.
type UpstreamConfig struct {
	// Name is the routing and metrics identifier. It must not collide with a
	// simulated provider's name.
	Name string `json:"name"`

	// BaseURL is the OpenAI-compatible root, including any version segment:
	// http://ollama:11434/v1, http://vllm:8000/v1, and so on.
	BaseURL string `json:"base_url"`

	// Model is the model identifier this upstream serves it under.
	Model string `json:"model"`

	// APIKeyEnv names an environment variable holding a bearer token. The key
	// itself is deliberately not a config field: a config file is a thing
	// people commit, and a secret scanner is a poor substitute for never
	// having somewhere to put the secret.
	APIKeyEnv string `json:"api_key_env"`

	// Priority orders this upstream under the failover policy, on the same
	// scale as the simulated providers: lower is preferred.
	Priority int `json:"priority"`

	// Rates prices the upstream. Zero for anything self-hosted, which is the
	// truth rather than a placeholder: the cost of a local model is not
	// metered per token.
	Rates Rates `json:"rates"`

	// MaxConcurrent caps simultaneous streams, and for a real local model it
	// is the most important field here. A single-GPU box serves one or two
	// requests at a time; letting ten in does not make it faster, it makes
	// every one of them slow. Exceeding the cap is a capacity refusal, which
	// fails over without counting against health -- the same treatment the
	// simulated local provider gets, for the same reason. Zero means uncapped.
	MaxConcurrent int `json:"max_concurrent"`

	// StartTimeout bounds connect, response headers and first token together.
	// Past it the attempt is a timeout, which does count against health.
	StartTimeout Duration `json:"start_timeout"`

	// IdleTimeout bounds the gap between tokens once the stream is running.
	// A stream that stalls mid-answer cannot be rerouted -- the header is
	// already out -- but it must not hang the caller forever either.
	IdleTimeout Duration `json:"idle_timeout"`

	// ProbeTimeout bounds the health probe.
	ProbeTimeout Duration `json:"probe_timeout"`
}

// Default timeouts. A local model on a laptop can spend ten seconds loading
// weights into memory on its first request, so the start budget is generous;
// past twenty seconds something is wrong rather than slow.
const (
	defaultStartTimeout = 20 * time.Second
	defaultIdleTimeout  = 30 * time.Second
	defaultProbeTimeout = 3 * time.Second
)

// NewUpstream builds an upstream from config, filling defaults and rejecting
// configuration that could only fail later at request time.
func NewUpstream(cfg UpstreamConfig, key string) (*Upstream, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("upstream: name is required")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("upstream %q: base_url is required", cfg.Name)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("upstream %q: model is required", cfg.Name)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = Duration(defaultStartTimeout)
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = Duration(defaultIdleTimeout)
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = Duration(defaultProbeTimeout)
	}

	return &Upstream{
		cfg:    cfg,
		apiKey: key,
		client: &http.Client{
			// No Timeout: a completion stream legitimately stays open for as
			// long as the answer takes, and a client-wide deadline would cut
			// a slow provider off for being slow, which is the failure mode
			// the gateway exists to route around rather than to cause.
			// Deadlines are applied per phase instead, in Start and Next.
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// Name implements Provider.
func (u *Upstream) Name() string { return u.cfg.Name }

// Rates implements Provider.
func (u *Upstream) Rates() Rates { return u.cfg.Rates }

// Model reports the model identifier this upstream was configured with.
func (u *Upstream) Model() string { return u.cfg.Model }

// Inflight reports how many streams are open right now. The admin snapshot and
// the inflight gauge read this, so a real upstream shows up on the dashboard
// with the same two numbers a simulated one does.
func (u *Upstream) Inflight() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inflight
}

// Capacity reports the configured concurrency cap, or 0 for uncapped.
func (u *Upstream) Capacity() int { return u.cfg.MaxConcurrent }

func (u *Upstream) acquire() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cfg.MaxConcurrent > 0 && u.inflight >= u.cfg.MaxConcurrent {
		return &Failure{
			Provider: u.cfg.Name,
			Kind:     KindCapacity,
			Message:  fmt.Sprintf("no capacity: %d of %d slots busy", u.inflight, u.cfg.MaxConcurrent),
		}
	}
	u.inflight++
	return nil
}

func (u *Upstream) release() {
	u.mu.Lock()
	if u.inflight > 0 {
		u.inflight--
	}
	u.mu.Unlock()
}

// ── wire types ───────────────────────────────────────────────────────────────

// Message is one turn of a chat transcript. It exists so that a caller using
// the OpenAI-compatible endpoint keeps its roles all the way to the upstream
// instead of having them flattened into a single string on the way through.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatCompletionChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ── Start ────────────────────────────────────────────────────────────────────

// Start implements Provider. See the type comment for why the first token is
// read here rather than on the first Next.
func (u *Upstream) Start(ctx context.Context, req Request) (Stream, error) {
	if ctx.Err() != nil {
		return nil, &Failure{Provider: u.cfg.Name, Kind: KindTimeout, Message: "canceled before start"}
	}

	// Capacity before the network. A refusal that costs a round trip is a
	// refusal that made the outage worse.
	if err := u.acquire(); err != nil {
		return nil, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			u.release()
		}
	}()

	body, err := json.Marshal(chatCompletionRequest{
		Model:         u.cfg.Model,
		Messages:      messagesFor(req),
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: fmt.Sprintf("encode request: %v", err)}
	}

	// The request context outlives Start, because it governs the whole stream.
	// The start budget is enforced by a watchdog that cancels it, which is then
	// rearmed as an idle timeout for the rest of the stream.
	reqCtx, cancel := context.WithCancel(ctx)
	watchdog := time.AfterFunc(time.Duration(u.cfg.StartTimeout), cancel)
	settled := false
	defer func() {
		if !settled {
			watchdog.Stop()
			cancel()
		}
	}()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: fmt.Sprintf("build request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if u.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+u.apiKey)
	}

	resp, err := u.client.Do(httpReq)
	if err != nil {
		return nil, u.transportFailure(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		f := u.statusFailure(resp)
		_ = resp.Body.Close()
		return nil, f
	}

	br := bufio.NewReaderSize(resp.Body, 8<<10)
	st := &upstreamStream{
		up:           u,
		cancel:       cancel,
		watchdog:     watchdog,
		body:         resp.Body,
		br:           br,
		promptTokens: EstimatePromptTokens(req.Prompt),
	}

	// Read forward to the first token. Everything before it -- role-only
	// deltas, empty keepalives, comment lines -- is protocol noise.
	text, err := st.readContent()
	if err != nil {
		_ = resp.Body.Close()
		if errors.Is(err, io.EOF) {
			// A 200 that produced no tokens at all. Nothing was streamed, so
			// this is still reroutable, and an empty answer is a failure of
			// the upstream rather than of the request.
			return nil, &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: "upstream returned an empty completion"}
		}
		return nil, u.transportFailure(ctx, err)
	}

	// The first token landed inside the budget. From here the stream is judged
	// on the gap between tokens instead.
	watchdog.Reset(time.Duration(u.cfg.IdleTimeout))

	// Resetting a timer that has already fired re-arms it; it does not undo the
	// call it already made. So the token may have arrived in the same instant
	// the start budget expired and canceled the request context, which would
	// hand back a Stream that is already dead. Returning the timeout instead
	// keeps that on the reroutable side of the boundary, where a request whose
	// first token never made it in time belongs.
	if reqCtx.Err() != nil {
		_ = resp.Body.Close()
		return nil, u.transportFailure(ctx, reqCtx.Err())
	}

	st.pending, st.hasPending = text, true
	settled, handedOff = true, true
	return st, nil
}

// messagesFor turns a gateway request into a chat transcript. A caller that
// supplied messages keeps them; one that supplied a bare prompt gets the single
// user turn that means the same thing.
func messagesFor(req Request) []Message {
	if len(req.Messages) > 0 {
		return req.Messages
	}
	return []Message{{Role: "user", Content: req.Prompt}}
}

// transportFailure classifies an error from the HTTP client or from reading the
// stream. The distinction that matters is timeout versus unavailable: both
// count against health, but only one of them means the upstream answered.
func (u *Upstream) transportFailure(parent context.Context, err error) *Failure {
	switch {
	case parent.Err() != nil:
		// The caller went away. Not this upstream's fault, and the router will
		// not get to try anyone else either.
		return &Failure{Provider: u.cfg.Name, Kind: KindTimeout, Message: "canceled by caller"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The watchdog fired: the upstream took the request and did not
		// produce a first token inside the budget.
		return &Failure{
			Provider: u.cfg.Name,
			Kind:     KindTimeout,
			Message:  fmt.Sprintf("no first token within %s", time.Duration(u.cfg.StartTimeout)),
		}
	default:
		return &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: err.Error()}
	}
}

// statusFailure maps an HTTP status onto the failure taxonomy. This is the only
// place the mapping lives, so a real 429 and a simulated one are the same kind
// of event by construction rather than by coincidence.
func (u *Upstream) statusFailure(resp *http.Response) *Failure {
	msg := errorMessage(resp)
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		// A working upstream shedding our load. Fails over; does not open a
		// circuit. See FailureKind.CountsAgainstHealth.
		return &Failure{
			Provider:   u.cfg.Name,
			Kind:       KindRateLimited,
			Message:    msg,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return &Failure{Provider: u.cfg.Name, Kind: KindTimeout, Message: msg}
	default:
		// Everything else -- 5xx, a 404 for a model that was never pulled, a
		// 401 for a key that expired -- is this upstream being unable to serve
		// us. All of it counts against health, which is right: none of it gets
		// better by sending the next request to the same place.
		return &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: msg}
	}
}

// errorMessage extracts something readable from an error response, bounded so a
// misconfigured endpoint returning a megabyte of HTML cannot fill a log line.
func errorMessage(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, envelope.Error.Message)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return fmt.Sprintf("upstream returned %d", resp.StatusCode)
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, text)
}

// retryAfter reads the header in both of the forms RFC 9110 allows.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ── Probe ────────────────────────────────────────────────────────────────────

// Probe implements Provider. It asks for the model list, which is the one route
// every OpenAI-compatible server implements and the cheapest thing it will
// answer.
//
// It also checks that the configured model is in that list. A server that is up
// but has never been given the weights answers a bare liveness check perfectly
// and then 404s every request, which is exactly the gap between "the probe
// passes" and "requests work" that makes probe-driven early recovery unsafe by
// default. Closing it here is what earns this upstream the right to be probed
// at all.
func (u *Upstream) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(u.cfg.ProbeTimeout))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.cfg.BaseURL+"/models", nil)
	if err != nil {
		return &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: fmt.Sprintf("build probe: %v", err)}
	}
	if u.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.apiKey)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Failure{Provider: u.cfg.Name, Kind: KindTimeout, Message: "health probe timed out"}
		}
		return &Failure{Provider: u.cfg.Name, Kind: KindUnavailable, Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return u.statusFailure(resp)
	}

	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil || len(list.Data) == 0 {
		// The server answered but not in a shape we can read. Treat that as
		// healthy rather than as an outage: the claim being made here is about
		// the upstream, and a list format we do not recognize is a fact about
		// this adapter.
		return nil
	}
	for _, m := range list.Data {
		if m.ID == u.cfg.Model {
			return nil
		}
	}
	return &Failure{
		Provider: u.cfg.Name,
		Kind:     KindUnavailable,
		Message:  fmt.Sprintf("model %q is not loaded on this upstream", u.cfg.Model),
	}
}

// ── the stream ───────────────────────────────────────────────────────────────

type upstreamStream struct {
	up       *Upstream
	cancel   context.CancelFunc
	watchdog *time.Timer
	body     io.ReadCloser
	br       *bufio.Reader

	mu           sync.Mutex
	pending      string
	hasPending   bool
	emitted      int
	promptTokens int
	reported     *wireUsage
	done         bool
	closed       bool

	once sync.Once
}

var _ Stream = (*upstreamStream)(nil)

// Next implements Stream.
func (s *upstreamStream) Next(ctx context.Context) (Chunk, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Chunk{}, ErrStreamClosed
	}
	if s.hasPending {
		text := s.pending
		s.pending, s.hasPending = "", false
		idx := s.emitted
		s.emitted++
		s.mu.Unlock()
		return Chunk{Text: text, Index: idx}, nil
	}
	if s.done {
		s.mu.Unlock()
		return Chunk{}, io.EOF
	}
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Chunk{}, &Failure{Provider: s.up.Name(), Kind: KindTimeout, Message: "context canceled mid-stream"}
	}

	text, err := s.readContent()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Chunk{}, io.EOF
		}
		return Chunk{}, s.up.transportFailure(ctx, err)
	}

	// Each token restarts the idle clock: the guarantee past the first token is
	// only that the upstream keeps talking, not that it finishes by any time.
	s.watchdog.Reset(time.Duration(s.up.cfg.IdleTimeout))

	s.mu.Lock()
	idx := s.emitted
	s.emitted++
	s.mu.Unlock()
	return Chunk{Text: text, Index: idx}, nil
}

// readContent advances the server-sent event stream to the next chunk that
// carries text, recording any usage report on the way past. It returns io.EOF
// at the end of the completion.
func (s *upstreamStream) readContent() (string, error) {
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
				s.mu.Lock()
				s.done = true
				s.mu.Unlock()
				return "", io.EOF
			}
			if !errors.Is(err, io.EOF) {
				return "", err
			}
			// A final line with no newline after it: fall through and parse.
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue // keepalive or comment
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // event:, id:, retry: -- none of which we act on
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			s.mu.Lock()
			s.done = true
			s.mu.Unlock()
			return "", io.EOF
		}

		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// One unparseable frame is not a reason to fail a completion that
			// is otherwise arriving. A stream of them ends at the idle
			// timeout, which is the right way for this to fail.
			continue
		}
		if chunk.Usage != nil {
			s.mu.Lock()
			s.reported = chunk.Usage
			s.mu.Unlock()
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				return c.Delta.Content, nil
			}
		}
	}
}

// Usage implements Stream. Real token counts are preferred over estimates: an
// upstream that reports usage knows what its own tokenizer did, and the whole
// reason the simulated providers estimate is that they have no tokenizer to
// ask.
func (s *upstreamStream) Usage() Usage {
	s.mu.Lock()
	prompt, completion := s.promptTokens, s.emitted
	if s.reported != nil {
		prompt, completion = s.reported.PromptTokens, s.reported.CompletionTokens
	}
	s.mu.Unlock()
	return Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CostUSD:          s.up.Rates().Cost(prompt, completion),
	}
}

// Close implements Stream.
func (s *upstreamStream) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		s.watchdog.Stop()
		// Canceling the request context is what tells the upstream to stop
		// generating. Without it, closing early leaves a model producing
		// tokens for a caller that has gone.
		s.cancel()
		_ = s.body.Close()
		s.up.release()
	})
	return nil
}
