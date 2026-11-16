package trace

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestInjectExtractRoundTrip(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})

	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	parent := Header(ctx)
	state := State(ctx)
	if !strings.Contains(parent, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q does not carry %s", parent, span.SpanContext().TraceID())
	}

	// A fresh context, as the async worker has after reading Redis.
	continued := Continue(context.Background(), parent, state)
	if ID(continued) != ID(ctx) {
		t.Fatalf("continued %s parent %s", ID(continued), ID(ctx))
	}

	childCtx, child := Start(continued, "async.attempt")
	defer child.End()
	if ID(childCtx) != ID(ctx) {
		t.Fatalf("child trace %s parent %s", ID(childCtx), ID(ctx))
	}
	if !child.SpanContext().IsValid() {
		t.Fatal("child span context is invalid")
	}
	link := trace.SpanContextFromContext(continued)
	if child.SpanContext().TraceID() != link.TraceID() {
		t.Fatal("child left the parent trace")
	}
}

func TestContinueEmptyIsIdentity(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "keep")
	got := Continue(ctx, "", "")
	if got.Value(struct{}{}) != "keep" {
		t.Fatal("empty traceparent dropped the context")
	}
	if ID(got) != "" {
		t.Fatalf("id %q", ID(got))
	}
}

func TestExporterURL(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4318")
	if got := exporterURL(); got != "http://otel-collector:4318/v1/traces" {
		t.Fatalf("url %s", got)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318/v1/traces")
	if got := exporterURL(); got != "http://localhost:4318/v1/traces" {
		t.Fatalf("url %s", got)
	}
}

func TestEnabledSwitch(t *testing.T) {
	t.Setenv("NIMBUS_TRACING", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	if !Enabled() {
		t.Fatal("default should trace")
	}
	for _, off := range []string{"0", "false", "off", "no"} {
		t.Setenv("NIMBUS_TRACING", off)
		if Enabled() {
			t.Fatalf("%s should disable tracing", off)
		}
	}
	t.Setenv("NIMBUS_TRACING", "1")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if Enabled() {
		t.Fatal("OTEL_SDK_DISABLED should disable tracing")
	}
}
