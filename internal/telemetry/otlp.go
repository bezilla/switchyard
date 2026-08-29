package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// newOTLPExporter builds an OTLP/HTTP trace exporter from the standard
// OTEL_EXPORTER_OTLP_* environment variables. Kept in its own file so the
// dependency on the OTLP packages is easy to see and easy to drop.
func newOTLPExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	return otlptracehttp.New(ctx)
}
