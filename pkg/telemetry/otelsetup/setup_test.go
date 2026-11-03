package otelsetup

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveSignal(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		env     map[string]string
		want    signalPlan
		wantErr string
	}{
		{name: "no endpoint stays off", want: signalPlan{}},
		{
			name: "generic endpoint defaults to grpc",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4317"},
			want: signalPlan{enabled: true, protocol: ProtocolGRPC},
		},
		{
			name: "per-signal endpoint and protocol",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost:4318/v1/traces",
				"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf",
				"OTEL_EXPORTER_OTLP_PROTOCOL":        "grpc",
			},
			want: signalPlan{enabled: true, protocol: ProtocolHTTP},
		},
		{
			name: "config overrides env protocol",
			cfg:  Config{Endpoint: "localhost:4318", Protocol: ProtocolHTTP},
			env:  map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			want: signalPlan{enabled: true, protocol: ProtocolHTTP},
		},
		{
			name: "sdk disabled wins over endpoint",
			cfg:  Config{Endpoint: "localhost:4317"},
			env:  map[string]string{"OTEL_SDK_DISABLED": "TRUE"},
			want: signalPlan{},
		},
		{
			name: "exporter none turns signal off",
			cfg:  Config{Endpoint: "localhost:4317"},
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "none"},
			want: signalPlan{},
		},
		{
			name:    "unsupported exporter errors",
			cfg:     Config{Endpoint: "localhost:4317"},
			env:     map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"},
			wantErr: "unsupported OTEL_TRACES_EXPORTER",
		},
		{
			name:    "unsupported protocol errors",
			cfg:     Config{Endpoint: "localhost:4317"},
			env:     map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"},
			wantErr: "unsupported OTLP protocol",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSignal(tc.cfg, envMap(tc.env), signalTraces)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSetup_DisabledReturnsNoopHandles(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	tel, shutdown, err := Setup(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if tel.Enabled {
		t.Fatal("Enabled = true with no endpoint configured")
	}
	if tel.Tracer == nil || tel.Meter == nil {
		t.Fatal("disabled Setup must still return non-nil no-op handles")
	}
	_, span := tel.Tracer.Start(context.Background(), "x")
	if span.IsRecording() {
		t.Fatal("no-op tracer produced a recording span")
	}
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("disabled shutdown: %v", err)
	}
}

func TestSetup_TracingInstallsW3CPropagators(t *testing.T) {
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	prev := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	tel, shutdown, err := Setup(context.Background(), Config{
		Endpoint: "localhost:4318", Insecure: true, Protocol: ProtocolHTTP,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer func() { _ = shutdown(ctx) }() // no collector running; flush errors are expected

	if !tel.Enabled {
		t.Fatal("Enabled = false with an endpoint configured")
	}
	fields := otel.GetTextMapPropagator().Fields()
	joined := strings.Join(fields, ",")
	if !strings.Contains(joined, "traceparent") || !strings.Contains(joined, "baggage") {
		t.Fatalf("propagator fields = %v, want traceparent and baggage", fields)
	}
}
