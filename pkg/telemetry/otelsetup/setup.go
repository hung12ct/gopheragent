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
// Endpoint and credentials follow the standard OTEL_* environment variables
// honored by the OTLP exporters (e.g. OTEL_EXPORTER_OTLP_ENDPOINT). Config
// fields, when set, take precedence over the environment.
package otelsetup

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// scopeName is the instrumentation scope reported on all gopheragent telemetry.
const scopeName = "github.com/hung12ct/gopheragent"

// Config controls the OTLP pipeline. The zero value is usable: it names the
// service "gopheragent" and reads the endpoint from the standard OTEL_*
// environment variables.
type Config struct {
	// ServiceName sets service.name on emitted telemetry. Empty falls back to
	// "gopheragent" (OTEL_SERVICE_NAME still overrides via the SDK if set).
	ServiceName string
	// Endpoint overrides OTEL_EXPORTER_OTLP_ENDPOINT, e.g. "localhost:4317".
	// Empty defers entirely to the environment.
	Endpoint string
	// Insecure disables transport security (plaintext gRPC). Use for local
	// collectors; leave false in production.
	Insecure bool
}

// Providers holds the ready-to-use Tracer and Meter handles for the gopheragent
// instrumentation scope.
type Providers struct {
	Tracer trace.Tracer
	Meter  metric.Meter
}

// Setup builds an OTLP TracerProvider and MeterProvider, registers them as the
// global OpenTelemetry providers, and returns Tracer/Meter handles plus a
// shutdown function that flushes and closes both exporters. Callers should defer
// the shutdown function so buffered spans and metrics are exported on exit.
func Setup(ctx context.Context, cfg Config) (*Providers, func(context.Context) error, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	tp, err := buildTracerProvider(ctx, cfg, res)
	if err != nil {
		return nil, nil, err
	}
	mp, err := buildMeterProvider(ctx, cfg, res)
	if err != nil {
		// Best-effort cleanup of the tracer provider already built.
		_ = tp.Shutdown(ctx)
		return nil, nil, err
	}

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	providers := &Providers{
		Tracer: tp.Tracer(scopeName),
		Meter:  mp.Meter(scopeName),
	}
	shutdown := func(ctx context.Context) error {
		terr := tp.Shutdown(ctx)
		merr := mp.Shutdown(ctx)
		if terr != nil {
			return fmt.Errorf("otelsetup: tracer shutdown: %w", terr)
		}
		if merr != nil {
			return fmt.Errorf("otelsetup: meter shutdown: %w", merr)
		}
		return nil
	}
	return providers, shutdown, nil
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

func buildTracerProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	opts := []otlptracegrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otelsetup: trace exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}

func buildMeterProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	opts := []otlpmetricgrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otelsetup: metric exporter: %w", err)
	}
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	), nil
}
