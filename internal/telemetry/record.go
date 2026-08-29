package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bezilla/switchyard/internal/breaker"
	"github.com/bezilla/switchyard/internal/provider"
	"github.com/bezilla/switchyard/internal/router"
)

// RequestOutcome is the terminal outcome of a gateway request, as opposed to
// the outcome of one attempt on one provider.
type RequestOutcome string

const (
	// OutcomeSuccess is a request that streamed to completion.
	OutcomeSuccess RequestOutcome = "success"

	// OutcomeNoProvider is a request that no provider would take. This is the
	// number the availability SLO is built on.
	OutcomeNoProvider RequestOutcome = "no_provider"

	// OutcomeStreamError is a request that started and then broke. It counts
	// against availability too: the caller did not get an answer.
	OutcomeStreamError RequestOutcome = "stream_error"

	// OutcomeCanceled is the caller going away. Not the gateway's fault, and
	// deliberately excluded from the SLO.
	OutcomeCanceled RequestOutcome = "canceled"
)

// RecordRequest records one completed gateway request.
func (t *Telemetry) RecordRequest(ctx context.Context, d router.Decision, outcome RequestOutcome, dur time.Duration, u provider.Usage, ttft time.Duration) {
	attrs := []attribute.KeyValue{
		AttrPolicy.String(string(d.Policy)),
		AttrOutcome.String(string(outcome)),
		AttrProvider.String(providerLabel(d.Provider)),
	}
	t.requests.Add(ctx, 1, metric.WithAttributes(attrs...))
	t.duration.Record(ctx, dur.Seconds(), metric.WithAttributes(attrs...))

	if d.Provider == "" {
		return
	}
	p := metric.WithAttributes(AttrProvider.String(d.Provider))
	if ttft > 0 {
		t.ttft.Record(ctx, ttft.Seconds(), p)
	}
	if u.PromptTokens > 0 {
		t.tokens.Add(ctx, int64(u.PromptTokens), metric.WithAttributes(
			AttrProvider.String(d.Provider), AttrKind.String("prompt")))
	}
	if u.CompletionTokens > 0 {
		t.tokens.Add(ctx, int64(u.CompletionTokens), metric.WithAttributes(
			AttrProvider.String(d.Provider), AttrKind.String("completion")))
	}
	if u.CostUSD > 0 {
		t.cost.Add(ctx, u.CostUSD, p)
	}
}

// RecordShed records a request the load generator could not issue.
func (t *Telemetry) RecordShed(ctx context.Context) {
	t.shedTokens.Add(ctx, 1)
}

// providerLabel keeps the label cardinality honest: a request nobody served
// still needs a value in the provider dimension, and an empty string reads as
// a bug when it appears in a legend.
func providerLabel(name string) string {
	if name == "" {
		return "none"
	}
	return name
}

// Observer adapts Telemetry to router.Observer.
func (t *Telemetry) Observer() router.Observer { return routerObserver{t: t} }

type routerObserver struct{ t *Telemetry }

func (o routerObserver) Attempted(a router.Attempt) {
	attrs := []attribute.KeyValue{
		AttrProvider.String(a.Provider),
		AttrOutcome.String(string(a.Outcome)),
	}
	if a.Kind != "" {
		attrs = append(attrs, AttrKind.String(a.Kind))
	} else {
		attrs = append(attrs, AttrKind.String("none"))
	}
	o.t.attempts.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

func (o routerObserver) Decided(router.Decision) {
	// The per-request counters are recorded by RecordRequest, which also knows
	// the terminal outcome. Recording here as well would double count.
}

func (o routerObserver) FailedOver(from, to string) {
	o.t.failovers.Add(context.Background(), 1, metric.WithAttributes(
		AttrFrom.String(from), AttrTo.String(to)))
}

// StateSource is what the observable gauges read from each collection.
type StateSource interface {
	// Targets returns the routable providers.
	Targets() []*router.Target
}

// RegisterGauges installs observable gauges for provider and breaker state.
// These are read at scrape time rather than pushed, because they describe a
// condition that exists between requests rather than an event that happened.
func (t *Telemetry) RegisterGauges(src StateSource) error {
	state, err := t.meter.Int64ObservableGauge("switchyard.breaker.state",
		metric.WithDescription("Circuit breaker state: 0 closed, 1 recovering, 2 open."))
	if err != nil {
		return err
	}
	admit, err := t.meter.Float64ObservableGauge("switchyard.breaker.admit_ratio",
		metric.WithDescription("Fraction of traffic the breaker currently admits. Between 0 and 1 during gradual recovery."))
	if err != nil {
		return err
	}
	healthy, err := t.meter.Int64ObservableGauge("switchyard.provider.healthy",
		metric.WithDescription("1 when the provider's last health probe succeeded, 0 otherwise."))
	if err != nil {
		return err
	}
	inflight, err := t.meter.Int64ObservableGauge("switchyard.provider.inflight",
		metric.WithDescription("Streams currently open against the provider."))
	if err != nil {
		return err
	}
	capacity, err := t.meter.Int64ObservableGauge("switchyard.provider.capacity",
		metric.WithDescription("Configured concurrency cap, or 0 when uncapped."))
	if err != nil {
		return err
	}

	_, err = t.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, tgt := range src.Targets() {
			p := metric.WithAttributes(AttrProvider.String(tgt.Name()))

			st := tgt.Breaker.State()
			o.ObserveInt64(state, breakerCode(st), p)
			o.ObserveFloat64(admit, tgt.Breaker.AdmitRatio(), p)

			_, probeErr := tgt.LastProbe()
			var up int64 = 1
			if probeErr != nil {
				up = 0
			}
			o.ObserveInt64(healthy, up, p)

			if s, ok := tgt.Provider.(interface {
				Inflight() int
				Capacity() int
			}); ok {
				o.ObserveInt64(inflight, int64(s.Inflight()), p)
				o.ObserveInt64(capacity, int64(s.Capacity()), p)
			}
		}
		return nil
	}, state, admit, healthy, inflight, capacity)
	return err
}

// breakerCode orders the states by how much traffic they block, so that a
// dashboard sorting on the value sorts by severity.
func breakerCode(s breaker.State) int64 {
	switch s {
	case breaker.Closed:
		return 0
	case breaker.Recovering:
		return 1
	case breaker.Open:
		return 2
	default:
		return 0
	}
}
