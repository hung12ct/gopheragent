// Package otelsetup wires an OpenTelemetry OTLP pipeline for gopheragent with a
// single call. It is the ONLY package in the repository that imports the
// OpenTelemetry SDK and OTLP exporters, so a library consumer that does not need
// export pays nothing: the core (pkg/agent, pkg/telemetry/otelllm,
// pkg/telemetry/oteltools) depends only on the OTel API.
//
// Usage:
//
//	tel, shutdown, err := otelsetup.Setup(ctx, otelsetup.Config{ServiceName: "my-agent"})
//	if err != nil { ... }
//	defer shutdown(context.Background())
//
//	llm := otelllm.NewProvider(p, otelllm.WithTracer(tel.Tracer), otelllm.WithMeter(tel.Meter), ...)
//	loop := agent.New(sm, reg, llm, agent.WithTracer(tel.Tracer), agent.WithMeter(tel.Meter))
//
// Telemetry is off unless an endpoint is configured (Config.Endpoint or the
// standard OTEL_EXPORTER_OTLP_[<SIGNAL>_]ENDPOINT variables). It also honors
// OTEL_SDK_DISABLED=true, OTEL_TRACES_EXPORTER / OTEL_METRICS_EXPORTER
// ("otlp" or "none"), and OTEL_EXPORTER_OTLP_[<SIGNAL>_]PROTOCOL ("grpc" or
// "http/protobuf"). Config fields, when set, take precedence over the
// environment. A disabled signal yields a no-op handle, so callers wire the
// returned Tracer/Meter unconditionally.
//
// When tracing is enabled, Setup also installs the W3C TraceContext + Baggage
// propagators globally so an agent run joins the caller's distributed trace
// (extract inbound headers with otel.GetTextMapPropagator()).
package otelsetup

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	gasemconv "github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

// scopeName is the instrumentation scope reported on all gopheragent telemetry.
const scopeName = gasemconv.ScopeName

// Config controls the OTLP pipeline. The zero value is usable: it names the
// service "gopheragent" and reads everything else from the standard OTEL_*
// environment variables.
type Config struct {
	// ServiceName sets service.name on emitted telemetry. Empty falls back to
	// "gopheragent" (OTEL_SERVICE_NAME still overrides via the SDK if set).
	ServiceName string
	// Endpoint overrides OTEL_EXPORTER_OTLP_ENDPOINT as a host:port, e.g.
	// "localhost:4317" (gRPC) or "localhost:4318" (HTTP). Empty defers to the
	// environment; with no endpoint anywhere, export stays off.
	Endpoint string
	// Insecure disables transport security (plaintext). Use for local
	// collectors; leave false in production.
	Insecure bool
	// Protocol overrides OTEL_EXPORTER_OTLP_[<SIGNAL>_]PROTOCOL: ProtocolGRPC
	// (the default) or ProtocolHTTP.
	Protocol string
}

// Providers holds the ready-to-use Tracer and Meter handles for the gopheragent
// instrumentation scope. Both are always non-nil: a disabled signal gets a
// no-op implementation.
type Providers struct {
	Tracer trace.Tracer
	Meter  metric.Meter
	// Enabled reports whether at least one signal exports. Callers that want
	// strictly zero instrumentation cost when off can skip wiring on false.
	Enabled bool
}

// Setup resolves the export plan from cfg and the environment, builds an OTLP
// TracerProvider and/or MeterProvider for the enabled signals, registers them
// (plus the W3C propagators when tracing) as the global OpenTelemetry
// providers, and returns handles plus a shutdown function that flushes and
// closes the exporters. Callers should defer the shutdown function so buffered
// spans and metrics are exported on exit; it is safe to call when disabled.
func Setup(ctx context.Context, cfg Config) (*Providers, func(context.Context) error, error) {
	tracesPlan, err := resolveSignal(cfg, os.Getenv, signalTraces)
	if err != nil {
		return nil, nil, err
	}
	metricsPlan, err := resolveSignal(cfg, os.Getenv, signalMetrics)
	if err != nil {
		return nil, nil, err
	}
	providers := &Providers{
		Tracer: tracenoop.NewTracerProvider().Tracer(scopeName),
		Meter:  metricnoop.NewMeterProvider().Meter(scopeName),
	}
	if !tracesPlan.enabled && !metricsPlan.enabled {
		return providers, func(context.Context) error { return nil }, nil
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	tp, mp, err := buildProviders(ctx, cfg, tracesPlan, metricsPlan, res)
	if err != nil {
		return nil, nil, err
	}

	// Register globals only once every provider built, so a failed Setup
	// leaves the process-wide OTel state untouched.
	if tp != nil {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}))
		providers.Tracer = tp.Tracer(scopeName)
	}
	if mp != nil {
		otel.SetMeterProvider(mp)
		providers.Meter = mp.Meter(scopeName)
	}
	providers.Enabled = true
	return providers, shutdownFunc(tp, mp), nil
}

// buildProviders constructs the SDK provider for each enabled signal; a
// disabled signal returns nil. On a metric failure the already-built tracer
// provider is shut down best-effort.
func buildProviders(ctx context.Context, cfg Config, tracesPlan, metricsPlan signalPlan, res *resource.Resource) (*sdktrace.TracerProvider, *sdkmetric.MeterProvider, error) {
	var tp *sdktrace.TracerProvider
	if tracesPlan.enabled {
		exp, err := newTraceExporter(ctx, cfg, tracesPlan.protocol)
		if err != nil {
			return nil, nil, err
		}
		tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	}
	var mp *sdkmetric.MeterProvider
	if metricsPlan.enabled {
		exp, err := newMetricExporter(ctx, cfg, metricsPlan.protocol)
		if err != nil {
			if tp != nil {
				_ = tp.Shutdown(ctx)
			}
			return nil, nil, err
		}
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
			sdkmetric.WithResource(res),
		)
	}
	return tp, mp, nil
}

// shutdownFunc flushes both providers regardless of the first error and
// reports both — a meter flush failure must not be masked by a tracer one.
func shutdownFunc(tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider) func(context.Context) error {
	return func(ctx context.Context) error {
		var terr, merr error
		if tp != nil {
			if err := tp.Shutdown(ctx); err != nil {
				terr = fmt.Errorf("otelsetup: tracer shutdown: %w", err)
			}
		}
		if mp != nil {
			if err := mp.Shutdown(ctx); err != nil {
				merr = fmt.Errorf("otelsetup: meter shutdown: %w", err)
			}
		}
		return errors.Join(terr, merr)
	}
}

func buildResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	name := cfg.ServiceName
	if name == "" {
		name = "gopheragent"
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(name)),
	)
	if err != nil {
		return nil, fmt.Errorf("otelsetup: build resource: %w", err)
	}
	return res, nil
}

func newTraceExporter(ctx context.Context, cfg Config, protocol string) (sdktrace.SpanExporter, error) {
	var (
		exp sdktrace.SpanExporter
		err error
	)
	if protocol == ProtocolHTTP {
		var opts []otlptracehttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err = otlptracehttp.New(ctx, opts...)
	} else {
		var opts []otlptracegrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("otelsetup: trace exporter (%s): %w", protocol, err)
	}
	return exp, nil
}

func newMetricExporter(ctx context.Context, cfg Config, protocol string) (sdkmetric.Exporter, error) {
	var (
		exp sdkmetric.Exporter
		err error
	)
	if protocol == ProtocolHTTP {
		var opts []otlpmetrichttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exp, err = otlpmetrichttp.New(ctx, opts...)
	} else {
		var opts []otlpmetricgrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exp, err = otlpmetricgrpc.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("otelsetup: metric exporter (%s): %w", protocol, err)
	}
	return exp, nil
}
