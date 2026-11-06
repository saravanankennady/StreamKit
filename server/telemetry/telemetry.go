// Package telemetry exports OpenTelemetry traces of HTTP requests, when a collector is configured.
package telemetry

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Start begins exporting to an OTLP/HTTP collector. It returns a function that flushes and stops.
func Start(ctx context.Context, endpoint, nodeID, version string) (func(context.Context) error, error) {
	exporter, err := otlptracehttp.New(ctx, exporterOptions(endpoint)...)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "streamkit-server"),
			attribute.String("service.version", version),
			attribute.String("service.instance.id", nodeID),
		)),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return provider.Shutdown, nil
}

func exporterOptions(endpoint string) []otlptracehttp.Option {
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	if strings.HasPrefix(endpoint, "http://") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts
}

// Wrap traces requests. Paths are not used as span names: they contain room names and identities, which are
// customer data and would make for unbounded cardinality.
func Wrap(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + routeGroup(r.URL.Path) }),
	)
}

func routeGroup(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case parts[0] == "rtc":
		return "/rtc"
	case parts[0] == "twirp" && len(parts) >= 3:
		return "/twirp/" + parts[1] + "/" + parts[2]
	case parts[0] == "api" && len(parts) >= 3:
		return "/api/v1/" + parts[2]
	}
	return "/" + parts[0]
}
