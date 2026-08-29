// Package provider defines the inference provider contract and the simulated
// providers Switchyard routes across.
//
// The contract is deliberately narrow. A provider starts a stream or fails; it
// never partially succeeds. That boundary is what makes safe failover possible:
// see Start.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Request is one inference call. It is intentionally provider-neutral: the
// gateway routes on shape and size, not on any provider's wire format.
type Request struct {
	// Model is the logical model name the caller asked for. Providers map it
	// onto whatever they actually serve.
	Model string

	// Prompt is the input text. Its length drives simulated prompt token
	// counts, so identical prompts cost identically on a given provider.
	Prompt string

	// MaxTokens caps the completion length.
	MaxTokens int

	// Key identifies the caller for rate-limit accounting. Empty means the
	// anonymous bucket.
	Key string
}

// Chunk is one streamed piece of a completion.
type Chunk struct {
	Text string

	// Index is the zero-based position of this chunk in the stream.
	Index int
}

// Usage reports what a completed stream consumed. Token counts are simulated
// but stable for a given request, and cost is derived from them by the
// provider's own published-style rates.
type Usage struct {
	PromptTokens     int
	CompletionTokens int

	// CostUSD is an estimate, not a bill. See the Limitations section of the
	// README: real providers meter differently and price changes.
	CostUSD float64
}

// Stream is an in-flight completion. Next returns io.EOF when the completion
// is finished, at which point Usage reports the totals.
//
// A Stream must be closed exactly once. Close after io.EOF is not an error.
type Stream interface {
	// Next blocks until the next chunk is ready, the context is canceled, or
	// the stream ends with io.EOF.
	Next(ctx context.Context) (Chunk, error)

	// Usage reports totals. It is only meaningful after Next has returned
	// io.EOF; before that the completion counts are partial.
	Usage() Usage

	// Close releases the stream. Safe to call more than once.
	Close() error
}

// Provider is one upstream that can serve inference.
//
// Start is where the failover boundary lives. A provider that cannot serve a
// request must fail from Start, before any chunk exists, so the router can try
// somebody else without the caller ever knowing. Once Start returns a Stream,
// the first token may already be on its way to the client and the request is
// committed to that provider: an error from Next is a failed request, not a
// reroutable one. Every simulated failure mode is therefore surfaced at Start.
type Provider interface {
	// Name is the stable identifier used in metrics, logs and routing config.
	Name() string

	// Start begins a completion or explains why it cannot.
	Start(ctx context.Context, req Request) (Stream, error)

	// Rates returns this provider's price per million tokens, used both for
	// cost accounting after the fact and for cost-aware routing before it.
	Rates() Rates

	// Probe is the health check: cheap, side-effect free, and independent of
	// whether any request happens to be in flight.
	Probe(ctx context.Context) error
}

// Rates prices a provider in US dollars per million tokens.
type Rates struct {
	PromptUSDPerMTok     float64 `json:"prompt_usd_per_mtok"`
	CompletionUSDPerMTok float64 `json:"completion_usd_per_mtok"`
}

// Cost estimates the dollar cost of a usage at these rates.
func (r Rates) Cost(promptTokens, completionTokens int) float64 {
	const perMillion = 1_000_000.0
	return float64(promptTokens)/perMillion*r.PromptUSDPerMTok +
		float64(completionTokens)/perMillion*r.CompletionUSDPerMTok
}

// EstimateFor prices a request before it runs, which is what cost-aware routing
// needs. The completion length is unknown up front, so this assumes the request
// runs to MaxTokens: the pessimistic estimate, which keeps the router from
// picking an expensive provider on an optimistic guess.
func (r Rates) EstimateFor(req Request) float64 {
	return r.Cost(EstimatePromptTokens(req.Prompt), req.MaxTokens)
}

// Failure classifies why a provider refused a request. The router treats these
// differently: a rate limit or capacity refusal is the provider working
// correctly and saying "not now", while an Unavailable is the provider being
// broken. Both trigger failover; only some of them should open a circuit.
type Failure struct {
	Provider string
	Kind     FailureKind
	Message  string

	// RetryAfter is set on rate limits, mirroring the header a real provider
	// would send.
	RetryAfter time.Duration
}

func (e *Failure) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s: %s: %s (retry after %s)", e.Provider, e.Kind, e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("%s: %s: %s", e.Provider, e.Kind, e.Message)
}

// FailureKind is the category of a Failure.
type FailureKind string

const (
	// KindUnavailable is a provider that is down or erroring: the upstream is
	// not working. These count against the circuit breaker.
	KindUnavailable FailureKind = "unavailable"

	// KindRateLimited is an HTTP 429 equivalent: the provider is healthy and
	// is shedding our load specifically. Failing over is right; opening a
	// circuit is not, because the provider is not broken.
	KindRateLimited FailureKind = "rate_limited"

	// KindCapacity is a local capacity refusal: no slot free right now. Like a
	// rate limit, this is correct behavior rather than breakage.
	KindCapacity FailureKind = "capacity"

	// KindTimeout is the provider taking longer than the caller allows.
	KindTimeout FailureKind = "timeout"
)

// CountsAgainstHealth reports whether a failure should push a circuit breaker
// toward open. Load shedding is not ill health: a rate-limited provider that
// tripped its own breaker would stay tripped precisely because we kept away
// from it, which is backwards.
func (k FailureKind) CountsAgainstHealth() bool {
	switch k {
	case KindUnavailable, KindTimeout:
		return true
	case KindRateLimited, KindCapacity:
		return false
	default:
		return true
	}
}

// KindOf extracts the FailureKind from an error, defaulting to KindUnavailable
// for errors that did not come from a provider.
func KindOf(err error) FailureKind {
	var f *Failure
	if errors.As(err, &f) {
		return f.Kind
	}
	return KindUnavailable
}

// NameOf extracts the provider name from an error, or "" if it did not come
// from a provider.
func NameOf(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Provider
	}
	return ""
}

// ErrStreamClosed is returned by Next after Close.
var ErrStreamClosed = errors.New("stream closed")

// Drain reads a stream to completion and returns the assembled text and usage.
// Callers that do not need incremental delivery use this; the HTTP handler does
// not, because the point of the demo is that tokens arrive as they are made.
func Drain(ctx context.Context, s Stream) (string, Usage, error) {
	defer func() { _ = s.Close() }()

	var text []byte
	for {
		chunk, err := s.Next(ctx)
		if errors.Is(err, io.EOF) {
			return string(text), s.Usage(), nil
		}
		if err != nil {
			return string(text), s.Usage(), err
		}
		text = append(text, chunk.Text...)
	}
}
