package otelsetup

import (
	"fmt"
	"strings"
)

// signalKind names an OTel signal the way the OTEL_* environment variables
// spell it (OTEL_TRACES_EXPORTER, OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, ...).
type signalKind string

const (
	signalTraces  signalKind = "TRACES"
	signalMetrics signalKind = "METRICS"
)

// OTLP transport protocols accepted by Config.Protocol and the
// OTEL_EXPORTER_OTLP_[<SIGNAL>_]PROTOCOL environment variables.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// signalPlan is the resolved export decision for one signal.
type signalPlan struct {
	enabled  bool
	protocol string
}

// resolveSignal decides, once at Setup time, whether a signal exports and over
// which protocol. A signal is off when OTEL_SDK_DISABLED=true, when its
// OTEL_<SIGNAL>_EXPORTER is "none", or when no endpoint is configured anywhere
// (Config.Endpoint, OTEL_EXPORTER_OTLP_ENDPOINT, or the per-signal endpoint) —
// so an unconfigured process never dials a default collector. getenv is
// injected so the resolution is testable without mutating the environment.
func resolveSignal(cfg Config, getenv func(string) string, kind signalKind) (signalPlan, error) {
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return signalPlan{}, nil
	}
	switch exp := strings.TrimSpace(getenv("OTEL_" + string(kind) + "_EXPORTER")); exp {
	case "", "otlp":
	case "none":
		return signalPlan{}, nil
	default:
		return signalPlan{}, fmt.Errorf("otelsetup: unsupported OTEL_%s_EXPORTER %q (want otlp or none)", kind, exp)
	}
	if cfg.Endpoint == "" &&
		getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		getenv("OTEL_EXPORTER_OTLP_"+string(kind)+"_ENDPOINT") == "" {
		return signalPlan{}, nil
	}
	proto := firstNonEmpty(
		cfg.Protocol,
		getenv("OTEL_EXPORTER_OTLP_"+string(kind)+"_PROTOCOL"),
		getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
		ProtocolGRPC,
	)
	if proto != ProtocolGRPC && proto != ProtocolHTTP {
		return signalPlan{}, fmt.Errorf("otelsetup: unsupported OTLP protocol %q for %s (want %s or %s)",
			proto, strings.ToLower(string(kind)), ProtocolGRPC, ProtocolHTTP)
	}
	return signalPlan{enabled: true, protocol: proto}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
