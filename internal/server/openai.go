package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bezilla/switchyard/internal/provider"
	"github.com/bezilla/switchyard/internal/router"
	"github.com/bezilla/switchyard/internal/telemetry"
)

// The OpenAI-compatible surface: POST /v1/chat/completions.
//
// It exists so that an existing client library can be pointed at the gateway by
// changing a base URL, which is the only form of "compatible" that is worth
// anything. Routing, the breaker, the failure taxonomy and the header-ordering
// guarantee are the same code as /v1/chat -- this file is a translation of
// request and response shapes and nothing else.
//
// STREAMING IS NOT IMPLEMENTED HERE, AND SAYS SO. A request carrying
// "stream": true is refused with 400 rather than quietly answered in one piece.
// That is the whole of the compatibility argument: the danger of a
// half-compatible endpoint is not the missing half, it is a caller that asks
// for something, appears to be given it, and finds out later. A client that
// wanted tokens as they were made and received one JSON object at the end has
// been misled about latency, about memory, and about what the gateway's
// failover guarantee covered on its behalf. An explicit refusal naming the
// roadmap item costs that caller one clear error and no illusions. See
// ROADMAP.md; streaming here waits on the same question v0.2 has to answer.

const openAIModelOwner = "switchyard"

// openAIChatRequest is the subset of the OpenAI chat-completions request the
// gateway acts on. Fields it does not implement are accepted and ignored,
// except stream, which is refused: silently ignoring that one is the failure
// mode described above.
type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`

	// MaxTokens is the older name; MaxCompletionTokens is the current one.
	// Clients in the wild send either, so both are read and the newer wins.
	MaxTokens           *int `json:"max_tokens"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
}

// openAIMessage is one turn. Content is raw because the field is a union: a
// plain string in most clients, an array of typed parts in the ones that
// support images and files.
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text flattens the content union to the text the gateway can route on.
func (m openAIMessage) text() (string, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return "", nil
	}

	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", errors.New("message content must be a string or an array of content parts")
	}

	var b strings.Builder
	for _, p := range parts {
		// Anything that is not text -- an image, an audio clip, a file
		// reference -- is dropped rather than silently stringified, and the
		// caller is told, because a request whose picture went missing should
		// not come back looking like it worked.
		if p.Type != "" && p.Type != "text" {
			return "", fmt.Errorf("content part of type %q is not supported", p.Type)
		}
		if b.Len() > 0 && p.Text != "" {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

type openAIChoice struct {
	Index        int              `json:"index"`
	Message      provider.Message `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`

	// Switchyard is the routing decision, in the response body as well as in
	// the headers. A client library that parses this shape into its own struct
	// throws unknown fields away, which is exactly why the headers exist -- but
	// a caller reading raw JSON should not have to go looking.
	Switchyard openAIRouting `json:"switchyard"`
}

type openAIRouting struct {
	Provider  string `json:"provider"`
	Policy    string `json:"policy"`
	Failovers int    `json:"failovers"`
}

// handleOpenAIChat serves POST /v1/chat/completions.
func (s *Server) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	var req openAIChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "",
			fmt.Sprintf("decode body: %v", err))
		return
	}

	if req.Stream {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "stream",
			"streaming is not implemented on /v1/chat/completions. Use POST /v1/chat, "+
				"which streams server-sent events, or send this request without \"stream\". "+
				"See ROADMAP.md.")
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages",
			"messages is required and must not be empty")
		return
	}

	msgs := make([]provider.Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		text, err := m.text()
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("messages[%d].content", i), err.Error())
			return
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, provider.Message{Role: role, Content: text})
	}

	maxTokens := 256
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		maxTokens = *req.MaxCompletionTokens
	}

	model := req.Model
	if model == "" {
		model = "default"
	}

	ctx, span := s.tel.Tracer.Start(r.Context(), "gateway.chat_completions")
	defer span.End()

	stream, decision, err := s.router.Route(ctx, provider.Request{
		Model:     model,
		Prompt:    flatten(msgs),
		Messages:  msgs,
		MaxTokens: maxTokens,
	})
	if err != nil {
		s.tel.RecordRequest(ctx, decision, telemetry.OutcomeNoProvider, time.Since(started), provider.Usage{}, 0)
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "",
			"no provider available")
		return
	}
	defer func() { _ = stream.Close() }()

	// The completion is assembled before anything is written, because a
	// non-streaming response is one object. That does NOT buy a wider failover
	// guarantee: a provider that dies at token forty is still a failed request
	// here, not a rerouted one. Retrying it elsewhere would give this endpoint
	// a different reliability contract from /v1/chat -- better on paper, and a
	// second behavior to document, test and reason about on a dashboard that
	// cannot tell the two endpoints apart. Uniform routing was worth more. The
	// argument against splicing two models' output is in ROADMAP.md and applies
	// here unchanged.
	var (
		text strings.Builder
		ttft time.Duration
	)

	for {
		chunk, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			outcome := telemetry.OutcomeStreamError
			status := http.StatusBadGateway
			if ctx.Err() != nil {
				outcome = telemetry.OutcomeCanceled
				status = 499 // client closed request; nothing will read it
			} else {
				s.log.Warn("stream failed mid-completion",
					"provider", decision.Provider, "error", err.Error(),
					"elapsed", time.Since(started).Round(time.Millisecond).String())
			}
			s.tel.RecordRequest(ctx, decision, outcome, time.Since(started), stream.Usage(), ttft)
			// Nothing has been written yet, so unlike the streaming endpoint
			// this can still return a real status code.
			s.openAIRoutingHeaders(w, decision)
			writeOpenAIError(w, status, "server_error", "",
				fmt.Sprintf("upstream %s failed mid-completion: %v", decision.Provider, err))
			return
		}
		if ttft == 0 {
			ttft = time.Since(started)
		}
		text.WriteString(chunk.Text)
	}

	usage := stream.Usage()
	finish := "stop"
	if usage.CompletionTokens >= maxTokens {
		finish = "length"
	}

	s.openAIRoutingHeaders(w, decision)
	writeJSON(w, http.StatusOK, openAIChatResponse{
		ID:      newCompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: strings.TrimSpace(text.String())},
			FinishReason: finish,
		}},
		Usage: openAIUsage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.PromptTokens + usage.CompletionTokens,
		},
		Switchyard: openAIRouting{
			Provider:  decision.Provider,
			Policy:    string(decision.Policy),
			Failovers: decision.Failovers,
		},
	})

	s.tel.RecordRequest(ctx, decision, telemetry.OutcomeSuccess, time.Since(started), usage, ttft)
}

// openAIRoutingHeaders writes the same X-Switchyard-* headers the streaming
// endpoint writes, in the same place in the sequence: after routing resolved
// and before any body. The ordering guarantee is not a property of one handler.
func (s *Server) openAIRoutingHeaders(w http.ResponseWriter, d router.Decision) {
	w.Header().Set("X-Switchyard-Provider", d.Provider)
	w.Header().Set("X-Switchyard-Policy", string(d.Policy))
	w.Header().Set("X-Switchyard-Failovers", fmt.Sprint(d.Failovers))
}

// handleOpenAIModels serves GET /v1/models: the routable providers, in the
// shape a client library expects to find a model list in.
//
// The gateway's "models" are its providers, which is the honest mapping. A
// caller choosing between them is choosing a route, and that is the only choice
// the gateway has to offer.
func (s *Server) handleOpenAIModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{{
		ID: "default", Object: "model", Created: 0, OwnedBy: openAIModelOwner,
	}}}
	for _, t := range s.router.Targets() {
		out.Data = append(out.Data, model{
			ID: t.Name(), Object: "model", Created: 0, OwnedBy: openAIModelOwner,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// flatten renders a transcript as the single string the simulated providers and
// the token estimator work from. Roles are kept so that the flattened form is
// not ambiguous between a two-turn conversation and one long message.
func flatten(msgs []provider.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
	}
	return b.String()
}

// newCompletionID mints the identifier clients log and correlate on.
func newCompletionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Reading from the kernel's entropy source does not fail in practice,
		// and an id is not a correctness property. A stable fallback beats
		// failing a completed request over its label.
		return "chatcmpl-switchyard"
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// writeOpenAIError writes the error envelope OpenAI clients parse.
func writeOpenAIError(w http.ResponseWriter, code int, kind, param, msg string) {
	type errBody struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param,omitempty"`
		Code    string `json:"code,omitempty"`
	}
	writeJSON(w, code, struct {
		Error errBody `json:"error"`
	}{Error: errBody{Message: msg, Type: kind, Param: param}})
}
