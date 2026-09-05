package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bezilla/switchyard/internal/provider"
)

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func decodeOpenAI(t *testing.T, rec *httptest.ResponseRecorder) openAIChatResponse {
	t.Helper()
	var out openAIChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestOpenAIChatCompletions(t *testing.T) {
	starts := 0
	h := newTestServer(t, &fakeProvider{name: "p0", starts: &starts})

	rec := postJSON(t, h, "/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeOpenAI(t, rec)
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Role != "assistant" {
		t.Fatalf("choices = %+v, want one assistant message", out.Choices)
	}
	if out.Choices[0].Message.Content == "" {
		t.Error("the completion is empty")
	}
	if out.Usage.TotalTokens != out.Usage.PromptTokens+out.Usage.CompletionTokens {
		t.Errorf("usage does not add up: %+v", out.Usage)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Errorf("id = %q, want a chatcmpl- prefix", out.ID)
	}
	// The routing decision reaches the caller both ways: a client library
	// parsing a fixed struct keeps the headers, a script reading raw JSON keeps
	// the body field.
	if got := rec.Header().Get("X-Switchyard-Provider"); got != "p0" {
		t.Errorf("X-Switchyard-Provider = %q, want p0", got)
	}
	if out.Switchyard.Provider != "p0" || out.Switchyard.Policy != "failover" {
		t.Errorf("switchyard block = %+v", out.Switchyard)
	}
}

// The recommendation this endpoint ships under: a caller that asks for
// streaming is refused, not quietly handed one object at the end. Half a
// compatibility surface is only dangerous when the missing half is silent.
func TestOpenAIChatCompletionsRefusesStreaming(t *testing.T) {
	starts := 0
	h := newTestServer(t, &fakeProvider{name: "p0", starts: &starts})

	rec := postJSON(t, h, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if out.Error.Param != "stream" {
		t.Errorf("error param = %q, want stream", out.Error.Param)
	}
	// The refusal has to name the thing that does stream, or it is a dead end.
	if !strings.Contains(out.Error.Message, "/v1/chat") {
		t.Errorf("error message does not point at the streaming endpoint: %q", out.Error.Message)
	}
	if starts != 0 {
		t.Errorf("%d provider(s) were asked to serve a request that was going to be refused", starts)
	}
}

func TestOpenAIChatCompletionsFailsOverAndReportsIt(t *testing.T) {
	starts := 0
	h := newTestServer(t,
		&fakeProvider{name: "p0", kind: provider.KindUnavailable, starts: &starts},
		&fakeProvider{name: "p1", kind: provider.KindRateLimited, starts: &starts},
		&fakeProvider{name: "p2", starts: &starts},
	)

	rec := postJSON(t, h, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Switchyard-Provider"); got != "p2" {
		t.Errorf("X-Switchyard-Provider = %q, want p2", got)
	}
	if got := rec.Header().Get("X-Switchyard-Failovers"); got != "2" {
		t.Errorf("X-Switchyard-Failovers = %q, want 2", got)
	}
	if out := decodeOpenAI(t, rec); out.Switchyard.Failovers != 2 {
		t.Errorf("body failover count = %d, want 2", out.Switchyard.Failovers)
	}
}

func TestOpenAIChatCompletionsNoProviderAvailable(t *testing.T) {
	starts := 0
	h := newTestServer(t, &fakeProvider{name: "p0", kind: provider.KindUnavailable, starts: &starts})

	rec := postJSON(t, h, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	// No provider accepted, so no provider is named. Naming one that refused
	// would send a reader chasing a provider that did nothing wrong.
	if got := rec.Header().Get("X-Switchyard-Provider"); got != "" {
		t.Errorf("X-Switchyard-Provider = %q, want empty", got)
	}
}

func TestOpenAIChatCompletionsValidation(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{"no messages", `{"messages":[]}`, "messages"},
		{"missing messages", `{"model":"x"}`, "messages"},
		{"unsupported content part", `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, "messages[0].content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			starts := 0
			h := newTestServer(t, &fakeProvider{name: "p0", starts: &starts})
			rec := postJSON(t, h, "/v1/chat/completions", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var out struct {
				Error struct {
					Param string `json:"param"`
				} `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out.Error.Param != tt.param {
				t.Errorf("error param = %q, want %q", out.Error.Param, tt.param)
			}
		})
	}
}

func TestOpenAIChatCompletionsAcceptsTextContentParts(t *testing.T) {
	starts := 0
	h := newTestServer(t, &fakeProvider{name: "p0", starts: &starts})
	rec := postJSON(t, h, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"again"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// The header-ordering guarantee is a property of the router and of where the
// handler writes, not of one endpoint. This is the same assertion the streaming
// endpoint gets, made against a provider that does real network I/O -- an
// OpenAI-compatible upstream behind the generic adapter -- so that the guarantee
// is checked on the path a real deployment uses.
func TestOpenAICompatibleUpstreamKeepsTheHeaderOrderingGuarantee(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"real \"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n"+
				"data: [DONE]\n\n")
	}))
	defer up.Close()

	upstream, err := provider.NewUpstream(provider.UpstreamConfig{
		Name: "real", BaseURL: up.URL + "/v1", Model: "m",
		StartTimeout: provider.Duration(5 * time.Second),
	}, "")
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	starts := 0
	// A broken simulated primary in front of the real upstream: the failover
	// hop is what puts the ordering guarantee under load.
	h := newTestServer(t,
		&fakeProvider{name: "broken", kind: provider.KindUnavailable, starts: &starts},
		upstream,
	)

	rec := &orderingRecorder{ResponseRecorder: httptest.NewRecorder(), starts: &starts}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat",
		strings.NewReader(`{"prompt":"hello","max_tokens":8}`))
	req.Header.Set("content-type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.providerAtHeader != "real" {
		t.Fatalf("header named %q when it was committed, want the real upstream that accepted",
			rec.providerAtHeader)
	}
	if !strings.Contains(rec.Body.String(), "real ") {
		t.Errorf("the upstream's tokens did not reach the client: %s", rec.Body.String())
	}
	// The real upstream's own token counts survive the trip to the caller.
	done := lastDoneEvent(t, rec.Body.String())
	if got, _ := done["prompt_tokens"].(float64); got != 7 {
		t.Errorf("prompt_tokens = %v, want the upstream's reported 7", done["prompt_tokens"])
	}
}

func TestOpenAIModelsListsTheRoutableProviders(t *testing.T) {
	starts := 0
	h := newTestServer(t, &fakeProvider{name: "p0", starts: &starts}, &fakeProvider{name: "p1", starts: &starts})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]bool, len(out.Data))
	for _, m := range out.Data {
		ids[m.ID] = true
	}
	for _, want := range []string{"default", "p0", "p1"} {
		if !ids[want] {
			t.Errorf("model list is missing %q: %v", want, ids)
		}
	}
}
