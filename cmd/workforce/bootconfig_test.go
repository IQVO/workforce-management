package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// setBootEnv sets a complete, valid mandatory environment; tests then blank
// or corrupt one variable at a time.
func setBootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:***@127.0.0.1:1/db?sslmode=disable")
	t.Setenv("MIGRATIONS_DATABASE_URL", "")
	t.Setenv("KAFKA_BROKERS", "127.0.0.1:1")
	t.Setenv("EVENT_PUBLISHER", "kafka")
	t.Setenv("PATH_CATALOGUE_SOURCE", "kafka")
	t.Setenv("LABOR_PERFORMANCE_MODE", "kafka-cache")
	t.Setenv("LABOR_PERFORMANCE_BASE_URL", "")
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func TestLoadBootConfigAcceptsACompleteEnvironment(t *testing.T) {
	setBootEnv(t)
	cfg, err := loadBootConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.databaseURL == "" || len(cfg.brokers) != 1 || cfg.laborMode != laborModeKafkaCache {
		t.Fatalf("config = %+v", cfg)
	}
}

// The three optional-looking variables default to the real behaviour: unset
// EVENT_PUBLISHER and PATH_CATALOGUE_SOURCE mean kafka, unset
// LABOR_PERFORMANCE_MODE means kafka-cache.
func TestLoadBootConfigDefaultsToTheRealBehaviour(t *testing.T) {
	setBootEnv(t)
	t.Setenv("EVENT_PUBLISHER", "")
	t.Setenv("PATH_CATALOGUE_SOURCE", "")
	t.Setenv("LABOR_PERFORMANCE_MODE", "")
	cfg, err := loadBootConfig()
	if err != nil {
		t.Fatalf("every default must be accepted: %v", err)
	}
	if cfg.laborMode != laborModeKafkaCache {
		t.Fatalf("laborMode = %q, want the kafka-cache default", cfg.laborMode)
	}
}

// http is the REST rollback of the labor-performance edge (R10): accepted
// with its base URL, refused without it.
func TestLoadBootConfigAcceptsTheHTTPRollbackWithItsBaseURL(t *testing.T) {
	setBootEnv(t)
	t.Setenv("LABOR_PERFORMANCE_MODE", "http")
	t.Setenv("LABOR_PERFORMANCE_BASE_URL", "http://labor-performance:80")
	cfg, err := loadBootConfig()
	if err != nil || cfg.laborMode != laborModeHTTP || cfg.laborBaseURL == "" {
		t.Fatalf("http rollback = %+v, %v", cfg, err)
	}
}

// Each missing or rejected value is a boot error that names its variable.
func TestLoadBootConfigRefusesEachMissingOrRejectedValue(t *testing.T) {
	cases := []struct {
		name, variable, value, want string
	}{
		{"database unset", "DATABASE_URL", "", "DATABASE_URL is required: this context runs on real Postgres"},
		{"brokers unset", "KAFKA_BROKERS", "", "KAFKA_BROKERS is required: this context runs on real Kafka"},
		{"brokers blank", "KAFKA_BROKERS", " , ", "KAFKA_BROKERS is required"},
		{"log publisher", "EVENT_PUBLISHER", "log", "EVENT_PUBLISHER=log is not supported: this context publishes to real Kafka"},
		{"unknown publisher", "EVENT_PUBLISHER", "rabbit", `unknown EVENT_PUBLISHER "rabbit"`},
		{"file catalogue", "PATH_CATALOGUE_SOURCE", "file", "PATH_CATALOGUE_SOURCE=file is not supported: this context runs on real Kafka"},
		{"unknown catalogue", "PATH_CATALOGUE_SOURCE", "s3", `unknown PATH_CATALOGUE_SOURCE "s3"`},
		{"permissive labor-performance", "LABOR_PERFORMANCE_MODE", "permissive", "LABOR_PERFORMANCE_MODE=permissive is not supported: this context runs on real Kafka"},
		{"unknown labor-performance", "LABOR_PERFORMANCE_MODE", "off", `unknown LABOR_PERFORMANCE_MODE "off"`},
		{"http without base url", "LABOR_PERFORMANCE_MODE", "http", "LABOR_PERFORMANCE_MODE=http requires LABOR_PERFORMANCE_BASE_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBootEnv(t)
			t.Setenv(tc.variable, tc.value)
			_, err := loadBootConfig()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s=%q: %v, want a boot error containing %q", tc.variable, tc.value, err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("the error does not name %s: %v", tc.variable, err)
			}
		})
	}
}

func TestLoadBootConfigReportsEveryProblemAtOnce(t *testing.T) {
	setBootEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("KAFKA_BROKERS", "")
	t.Setenv("EVENT_PUBLISHER", "log")
	t.Setenv("PATH_CATALOGUE_SOURCE", "file")
	t.Setenv("LABOR_PERFORMANCE_MODE", "permissive")
	_, err := loadBootConfig()
	if err == nil {
		t.Fatal("want a boot error")
	}
	for _, name := range []string{"DATABASE_URL", "KAFKA_BROKERS", "EVENT_PUBLISHER", "PATH_CATALOGUE_SOURCE", "LABOR_PERFORMANCE_MODE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
}

// run fails fast on a bad configuration: before it opens a connection and
// before it binds the listener.
func TestRunFailsFastBeforeBindingTheListener(t *testing.T) {
	setBootEnv(t)
	t.Setenv("DATABASE_URL", "")
	addr := freeAddr(t)
	t.Setenv("HTTP_ADDR", addr)

	start := time.Now()
	err := run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("run = %v, want a boot error naming DATABASE_URL", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a configuration error must not wait on any dependency")
	}
	if c, derr := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", addr); derr == nil {
		_ = c.Close()
		t.Fatal("the listener was bound although the configuration is invalid")
	}
}

func TestBuildMeasuredRateClientIsTheHTTPBreakerClient(t *testing.T) {
	c := buildMeasuredRateClient("http://labor-performance:80", nil, quietLogger())
	if c == nil {
		t.Fatal("no http measured-rate client")
	}
}
