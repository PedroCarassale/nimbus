// Package trace propagates W3C trace context and exports spans over OTLP.
//
// Tracing is on unless NIMBUS_TRACING is 0/false/off/no or OTEL_SDK_DISABLED
// is true. Export uses a batch processor: a collector that is down drops
// spans in the background and does not fail the invocation.
package trace

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const instrumentation = "nimbus"

// Enabled reports whether this process should export spans.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NIMBUS_TRACING"))) {
	case "0", "false", "off", "no":
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	return true
}

// Setup installs a global tracer provider. The returned function flushes and
// shuts the exporter down. It is safe to call when tracing is disabled.
func Setup(ctx context.Context) func(context.Context) error {
	noop := func(context.Context) error { return nil }
	otel.SetErrorHandler(rateHandler{})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if !Enabled() {
		log.Printf("[trace] tracing apagado (NIMBUS_TRACING)")
		return noop
	}

	endpoint := exporterURL()
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithTimeout(2*time.Second),
	)
	if err != nil {
		log.Printf("[trace] no se pudo crear el exporter OTLP (%s): %v", endpoint, err)
		return noop
	}

	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = "nimbus-compute-plane"
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", name)))
	if err != nil {
		log.Printf("[trace] resource: %v", err)
		res = resource.Empty()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(time.Second),
			sdktrace.WithExportTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(128),
			sdktrace.WithMaxQueueSize(2048),
		),
	)
	otel.SetTracerProvider(tp)
	log.Printf("[trace] OTLP %s service=%s", endpoint, name)
	return func(ctx context.Context) error {
		flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := tp.Shutdown(flushCtx); err != nil {
			log.Printf("[trace] shutdown: %v", err)
		}
		return nil
	}
}

func exporterURL() string {
	raw := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if raw == "" {
		raw = "http://otel-collector:4318"
	}
	raw = strings.TrimRight(raw, "/")
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	if !strings.HasSuffix(raw, "/v1/traces") {
		raw += "/v1/traces"
	}
	return raw
}

// Start begins a child span. Ending the span is the caller's job. When
// tracing is off the span is a no-op and End is safe.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer(instrumentation).Start(ctx, name, opts...)
}

// ExtractHTTP continues a trace from an incoming W3C traceparent header.
func ExtractHTTP(ctx context.Context, header http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(header))
}

// InjectHTTP writes the W3C traceparent of ctx onto an outgoing request.
func InjectHTTP(ctx context.Context, header http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
}

// Continue attaches a remote parent stored in Redis (or anywhere else) to ctx.
// Cancellation and values of ctx are preserved.
func Continue(ctx context.Context, traceparent, tracestate string) context.Context {
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{}
	carrier.Set("traceparent", traceparent)
	if tracestate != "" {
		carrier.Set("tracestate", tracestate)
	}
	extracted := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

// ID is the hex trace id, or empty when ctx has no valid span.
func ID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Header is the W3C traceparent for ctx, or empty.
func Header(ctx context.Context) string {
	parent, _ := inject(ctx)
	return parent
}

// State is the W3C tracestate for ctx, or empty.
func State(ctx context.Context) string {
	_, state := inject(ctx)
	return state
}

func inject(ctx context.Context) (string, string) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent"), carrier.Get("tracestate")
}

// Fail marks span as an error. A nil span is ignored.
func Fail(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Logf writes a process log line prefixed with the trace id when ctx has one.
func Logf(ctx context.Context, format string, args ...any) {
	if id := ID(ctx); id != "" {
		log.Printf("trace=%s "+format, append([]any{id}, args...)...)
		return
	}
	log.Printf(format, args...)
}

// rateHandler keeps a down collector from flooding the log.
type rateHandler struct{}

var (
	errMu   sync.Mutex
	errNext time.Time
)

func (rateHandler) Handle(err error) {
	if err == nil {
		return
	}
	errMu.Lock()
	defer errMu.Unlock()
	now := time.Now()
	if now.Before(errNext) {
		return
	}
	errNext = now.Add(30 * time.Second)
	log.Printf("[trace] export: %v", err)
}
