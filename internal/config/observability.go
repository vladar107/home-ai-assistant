package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultOTelServiceName          = "home-agent"
	defaultOTelMetricExportInterval = 60 * time.Second
	maximumOTelServiceNameRunes     = 128
)

type ObservabilityConfig struct {
	Enabled              bool
	ServiceName          string
	MetricExportInterval time.Duration
}

func defaultObservabilityConfig() ObservabilityConfig {
	return ObservabilityConfig{
		ServiceName:          defaultOTelServiceName,
		MetricExportInterval: defaultOTelMetricExportInterval,
	}
}

func loadObservabilityConfig(cfg *ObservabilityConfig) error {
	if value := os.Getenv("HOME_AGENT_OTEL_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_OTEL_ENABLED: must be true or false")
		}
		cfg.Enabled = enabled
	}

	if value := os.Getenv("OTEL_SERVICE_NAME"); value != "" {
		cfg.ServiceName = value
	}
	if value := os.Getenv("OTEL_METRIC_EXPORT_INTERVAL"); value != "" {
		milliseconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || milliseconds <= 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) {
			return fmt.Errorf("OTEL_METRIC_EXPORT_INTERVAL: must be a positive number of milliseconds")
		}
		cfg.MetricExportInterval = time.Duration(milliseconds) * time.Millisecond
	}

	return cfg.validate()
}

func (c ObservabilityConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if c.ServiceName != strings.TrimSpace(c.ServiceName) ||
		!utf8.ValidString(c.ServiceName) ||
		utf8.RuneCountInString(c.ServiceName) < 1 ||
		utf8.RuneCountInString(c.ServiceName) > maximumOTelServiceNameRunes ||
		strings.IndexFunc(c.ServiceName, unicode.IsControl) >= 0 {
		return fmt.Errorf("OTEL_SERVICE_NAME: must be trimmed, valid UTF-8, and between 1 and 128 characters")
	}
	if c.MetricExportInterval <= 0 {
		return fmt.Errorf("OTEL_METRIC_EXPORT_INTERVAL: must be greater than zero")
	}

	baseEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	traceEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	metricEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	if baseEndpoint == "" && (traceEndpoint == "" || metricEndpoint == "") {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: a shared endpoint or both signal-specific endpoints are required when OpenTelemetry is enabled")
	}
	for name, endpoint := range map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         baseEndpoint,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  traceEndpoint,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": metricEndpoint,
	} {
		if endpoint != "" {
			if err := validateOTLPEndpoint(name, endpoint); err != nil {
				return err
			}
		}
	}
	for name, protocol := range map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL":         os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL":  os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"),
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"),
	} {
		if protocol != "" && !strings.EqualFold(protocol, "http/protobuf") {
			return fmt.Errorf("%s: only http/protobuf is supported", name)
		}
	}

	return nil
}

func validateOTLPEndpoint(name, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s: must be a trimmed absolute HTTP or HTTPS URL", name)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return fmt.Errorf("%s: must be an absolute HTTP or HTTPS URL", name)
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("%s: must use HTTP or HTTPS", name)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%s: user information, query, and fragment are not allowed", name)
	}
	return nil
}
