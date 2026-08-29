// Package telemetry wires OpenTelemetry metrics and tracing for the gateway.
//
// Metrics are exported in Prometheus format from the gateway's own /metrics
// endpoint, so the demo stack is a scrape away from working and needs no
// collector. Tracing is instrumented unconditionally but only exported when an
// OTLP endpoint is configured; see DESIGN.md for why the compose stack does not
// ship a trace backend.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const scopeName = "github.com/bezilla/switchyard"

// Telemetry holds the instruments the gateway records to.
type Telemetry struct {
	// Registry is the Prometheus registry the /metrics handler serves.
	Registry *prometheus.Registry

	Tracer trace.Tracer

	requests   metric.Int64Counter
	duration   metric.Float64Histogram
	ttft       metric.Float64Histogram
	tokens     metric.Int64Counter
	cost       metric.Float64Counter
	attempts   metric.Int64Counter
	failovers  metric.Int64Counter
	shedTokens metric.Int64Counter

	meter metric.Meter

	shutdown []func(context.Context) error
}

// Attribute keys, named once so a typo cannot split a series in two.
var (
	AttrProvider = attribute.Key("provider")
	AttrPolicy   = attribute.Key("policy")
	AttrOutcome  = attribute.Key("outcome")
	AttrKind     = attribute.Key("kind")
	AttrFrom     = attribute.Key("from")
	AttrTo       = attribute.Key("to")
)

// New builds the telemetry pipeline. version is stamped onto the resource so a
// dashboard can tell two builds apart.
func New(ctx context.Context, version string) (*Telemetry, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("switchyard"),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		// The scope labels add a dimension to every series that says only
		// "this came from Switchyard", which the dashboards never group by.
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("build prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(exp),
		// Latency buckets span three orders of magnitude because that is the
		// real range here: a fast provider's first token lands near 100ms and
		// a slow one under injected latency runs past ten seconds. The default
		// buckets top out well below that and would flatten the tail into one
		// bar exactly when the tail is the story.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "switchyard.*.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{
					0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 7.5, 10, 20, 30,
				},
			}},
		)),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "switchyard.*.ttft"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{
					0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.4, 0.6, 1, 1.5, 2.5, 4, 6, 10,
				},
			}},
		)),
	)
	otel.SetMeterProvider(mp)

	t := &Telemetry{
		Registry: reg,
		meter:    mp.Meter(scopeName),
		shutdown: []func(context.Context) error{mp.Shutdown},
	}

	if err := t.instruments(); err != nil {
		return nil, err
	}
	if err := t.tracing(ctx, res); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Telemetry) instruments() error {
	var err error
	join := func(e error) {
		err = errors.Join(err, e)
	}

	var e error
	t.requests, e = t.meter.Int64Counter("switchyard.requests",
		metric.WithDescription("Inference requests handled by the gateway, by terminal outcome."))
	join(e)

	t.duration, e = t.meter.Float64Histogram("switchyard.request.duration",
		metric.WithDescription("End-to-end request duration, including every failover attempt."),
		metric.WithUnit("s"))
	join(e)

	t.ttft, e = t.meter.Float64Histogram("switchyard.stream.ttft",
		metric.WithDescription("Time to first token from the provider that served the request."),
		metric.WithUnit("s"))
	join(e)

	t.tokens, e = t.meter.Int64Counter("switchyard.tokens",
		metric.WithDescription("Tokens attributed to a provider, split into prompt and completion."))
	join(e)

	t.cost, e = t.meter.Float64Counter("switchyard.cost.usd",
		metric.WithDescription("Estimated spend in US dollars. An estimate from simulated rates, not a bill."))
	join(e)

	t.attempts, e = t.meter.Int64Counter("switchyard.routing.attempts",
		metric.WithDescription("Providers considered for a request, by how the attempt ended."))
	join(e)

	t.failovers, e = t.meter.Int64Counter("switchyard.failovers",
		metric.WithDescription("Hops from a provider that would not serve a request to the next candidate."))
	join(e)

	t.shedTokens, e = t.meter.Int64Counter("switchyard.load.shed",
		metric.WithDescription("Requests the load generator could not issue because the gateway was saturated."))
	join(e)

	return err
}

// tracing installs a tracer provider. Without an OTLP endpoint configured this
// is a no-op tracer: the spans are still written by the code, they just go
// nowhere, so turning on a trace backend later needs no code change.
func (t *Telemetry) tracing(ctx context.Context, res *resource.Resource) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		t.Tracer = noop.NewTracerProvider().Tracer(scopeName)
		return nil
	}

	exp, err := newOTLPExporter(ctx)
	if err != nil {
		return fmt.Errorf("build otlp trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
	)
	otel.SetTracerProvider(tp)
	t.Tracer = tp.Tracer(scopeName)
	t.shutdown = append(t.shutdown, tp.Shutdown)
	return nil
}

// Shutdown flushes and stops the pipeline.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var err error
	for _, fn := range t.shutdown {
		err = errors.Join(err, fn(ctx))
	}
	return err
}
