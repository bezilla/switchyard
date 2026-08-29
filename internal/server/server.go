// Package server exposes the gateway over HTTP: the inference endpoint, the
// metrics endpoint Prometheus scrapes, and the admin endpoints that the make
// targets drive.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/provider"
	"github.com/bezilla/switchyard/internal/router"
	"github.com/bezilla/switchyard/internal/telemetry"
)

// Controller is the subset of the load generator the admin API can drive. It is
// an interface so the server does not depend on the generator's internals.
type Controller interface {
	// SetRate changes the offered load in requests per second.
	SetRate(rps float64)

	// Rate reports the current offered load.
	Rate() float64
}

// Injector is a provider that accepts injected faults. Only the simulated
// providers implement it, which is the point: the admin surface can only break
// things that were built to be broken.
type Injector interface {
	Inject(provider.Injection)
	Injection() provider.Injection
}

// Server holds the gateway's HTTP surface.
type Server struct {
	router  *router.Router
	tel     *telemetry.Telemetry
	load    Controller
	log     *slog.Logger
	version string
}

// New builds the server. load may be nil, in which case the traffic admin
// endpoint reports that there is no generator to drive.
func New(r *router.Router, tel *telemetry.Telemetry, load Controller, log *slog.Logger, version string) *Server {
	return &Server{router: r, tel: tel, load: load, log: log, version: version}
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/chat", s.handleChat)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.tel.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mux.HandleFunc("GET /admin/state", s.handleState)
	mux.HandleFunc("POST /admin/inject", s.handleInject)
	mux.HandleFunc("POST /admin/policy", s.handlePolicy)
	mux.HandleFunc("POST /admin/traffic", s.handleTraffic)
	mux.HandleFunc("POST /admin/recovery", s.handleRecovery)

	return mux
}

// chatRequest is the gateway's request shape. It is deliberately not any
// vendor's schema: pretending to be a drop-in for a real API would promise
// compatibility this does not have.
type chatRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
}

// handleChat serves one inference request, streaming the completion as
// server-sent events.
//
// The response header is written only after a provider has accepted the
// request. That ordering is the whole reason failover is invisible to the
// caller: until the first byte goes out, the gateway is still free to change
// its mind about who serves this.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 256
	}
	if req.Model == "" {
		req.Model = "default"
	}

	ctx, span := s.tel.Tracer.Start(r.Context(), "gateway.chat")
	defer span.End()

	stream, decision, err := s.router.Route(ctx, provider.Request{
		Model:     req.Model,
		Prompt:    req.Prompt,
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		s.tel.RecordRequest(ctx, decision, telemetry.OutcomeNoProvider, time.Since(started), provider.Usage{}, 0)
		body, _ := json.Marshal(map[string]any{
			"error":    "no provider available",
			"attempts": decision.Attempts,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
		return
	}
	defer func() { _ = stream.Close() }()

	// Naming the chosen provider in a header means a caller can see the
	// routing decision without reading a dashboard, which is how you debug a
	// single bad request rather than an aggregate.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Switchyard-Provider", decision.Provider)
	w.Header().Set("X-Switchyard-Policy", string(decision.Policy))
	w.Header().Set("X-Switchyard-Failovers", fmt.Sprint(decision.Failovers))

	flusher, canFlush := w.(http.Flusher)
	var ttft time.Duration

	for {
		chunk, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			outcome := telemetry.OutcomeStreamError
			if errors.Is(ctx.Err(), context.Canceled) {
				outcome = telemetry.OutcomeCanceled
			}
			s.tel.RecordRequest(ctx, decision, outcome, time.Since(started), stream.Usage(), ttft)
			// The header is already out, so the only honest way to signal
			// failure now is to say so in the stream and stop.
			s.sse(w, "error", map[string]string{"error": err.Error()})
			if canFlush {
				flusher.Flush()
			}
			return
		}
		if ttft == 0 {
			ttft = time.Since(started)
		}
		s.sse(w, "chunk", map[string]any{"index": chunk.Index, "text": chunk.Text})
		if canFlush {
			flusher.Flush()
		}
	}

	usage := stream.Usage()
	s.sse(w, "done", map[string]any{
		"provider":           decision.Provider,
		"policy":             decision.Policy,
		"failovers":          decision.Failovers,
		"prompt_tokens":      usage.PromptTokens,
		"completion_tokens":  usage.CompletionTokens,
		"estimated_cost_usd": usage.CostUSD,
		"ttft_ms":            ttft.Milliseconds(),
	})
	if canFlush {
		flusher.Flush()
	}

	s.tel.RecordRequest(ctx, decision, telemetry.OutcomeSuccess, time.Since(started), usage, ttft)
}

func (s *Server) sse(w http.ResponseWriter, event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
}

// handleHealthz reports that the gateway process is up. It deliberately does
// not depend on provider health: a gateway with every upstream down is still
// running correctly and should not be restarted by an orchestrator for it.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

// handleReadyz reports whether any provider could serve a request right now.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	for _, t := range s.router.Targets() {
		if t.Breaker.State() != breaker.Open {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no provider available"})
}

// providerState is one provider's line in the admin snapshot.
type providerState struct {
	Name      string             `json:"name"`
	Priority  int                `json:"priority"`
	Breaker   any                `json:"breaker"`
	Injection provider.Injection `json:"injection"`
	Rates     provider.Rates     `json:"rates"`
	LastProbe string             `json:"last_probe"`
	ProbedAgo string             `json:"probed_ago,omitempty"`
	Inflight  int                `json:"inflight"`
	Capacity  int                `json:"capacity"`
}

// handleState returns everything the make targets and the e2e test need to
// assert on, in one document.
func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	out := struct {
		Version    string          `json:"version"`
		Policy     string          `json:"policy"`
		TrafficRPS float64         `json:"traffic_rps"`
		Providers  []providerState `json:"providers"`
	}{
		Version: s.version,
		Policy:  string(s.router.Policy()),
	}
	if s.load != nil {
		out.TrafficRPS = s.load.Rate()
	}

	for _, t := range s.router.Targets() {
		ps := providerState{
			Name:      t.Name(),
			Priority:  t.Priority,
			Breaker:   t.Breaker.Stats(),
			Rates:     t.Provider.Rates(),
			LastProbe: "ok",
		}
		if at, err := t.LastProbe(); err != nil {
			ps.LastProbe = err.Error()
			ps.ProbedAgo = time.Since(at).Truncate(time.Millisecond).String()
		} else if !at.IsZero() {
			ps.ProbedAgo = time.Since(at).Truncate(time.Millisecond).String()
		}
		if inj, ok := t.Provider.(Injector); ok {
			ps.Injection = inj.Injection()
		}
		if sim, ok := t.Provider.(interface {
			Inflight() int
			Capacity() int
		}); ok {
			ps.Inflight = sim.Inflight()
			ps.Capacity = sim.Capacity()
		}
		out.Providers = append(out.Providers, ps)
	}
	writeJSON(w, http.StatusOK, out)
}

type injectRequest struct {
	Provider   string  `json:"provider"`
	Mode       string  `json:"mode"`
	Rate       float64 `json:"rate"`
	SlowFactor float64 `json:"slow_factor"`
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req injectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}

	tgt := s.router.Target(req.Provider)
	if tgt == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown provider %q", req.Provider))
		return
	}
	inj, ok := tgt.Provider.(Injector)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("provider %q does not accept injected faults", req.Provider))
		return
	}

	mode := provider.Mode(req.Mode)
	switch mode {
	case provider.ModeHealthy, provider.ModeError, provider.ModeRateLimit, provider.ModeSlow:
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown mode %q: want healthy, error, ratelimit or slow", req.Mode))
		return
	}

	inj.Inject(provider.Injection{Mode: mode, Rate: req.Rate, SlowFactor: req.SlowFactor})
	s.log.Info("fault injected", "provider", req.Provider, "mode", mode, "rate", req.Rate)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":  req.Provider,
		"injection": inj.Injection(),
	})
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy string `json:"policy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if err := s.router.SetPolicy(router.Policy(req.Policy)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("routing policy changed", "policy", req.Policy)
	writeJSON(w, http.StatusOK, map[string]string{"policy": req.Policy})
}

func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if s.load == nil {
		writeError(w, http.StatusBadRequest, "no load generator is running")
		return
	}
	var req struct {
		RPS float64 `json:"rps"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if req.RPS < 0 {
		writeError(w, http.StatusBadRequest, "rps must not be negative")
		return
	}
	s.load.SetRate(req.RPS)
	s.log.Info("offered load changed", "rps", req.RPS)
	writeJSON(w, http.StatusOK, map[string]float64{"rps": s.load.Rate()})
}

// handleRecovery toggles probe-driven early recovery on every breaker.
//
// It exists so `make heal-apex` can turn the affordance on for the demo without
// a restart -- restarting mid-failover would destroy the state being
// demonstrated. The flag is off by default and this is the only thing that
// turns it on; see DESIGN.md for the flapping risk it accepts.
func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProbeEarlyRecovery *bool `json:"probe_early_recovery"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if req.ProbeEarlyRecovery == nil {
		writeError(w, http.StatusBadRequest, "probe_early_recovery is required")
		return
	}
	for _, t := range s.router.Targets() {
		t.Breaker.SetProbeEarlyRecovery(*req.ProbeEarlyRecovery)
	}
	s.log.Info("probe-driven early recovery changed", "enabled", *req.ProbeEarlyRecovery)
	writeJSON(w, http.StatusOK, map[string]bool{"probe_early_recovery": *req.ProbeEarlyRecovery})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
