package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

func setBootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:***@127.0.0.1:1/db?sslmode=disable")
	t.Setenv("KAFKA_BROKERS", "127.0.0.1:1")
	t.Setenv("EVENT_PUBLISHER", "kafka")
	t.Setenv("PATH_CATALOGUE_SOURCE", "kafka")
	t.Setenv("A2A_AGENT_URL", "http://workforce-management-a2a.warehouse-systems.svc.cluster.local:8093/")
}

func TestLoadBootConfigAcceptsACompleteEnvironment(t *testing.T) {
	setBootEnv(t)
	cfg, err := loadBootConfig()
	if err != nil || cfg.databaseURL == "" || len(cfg.brokers) != 1 || cfg.agentURL == "" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	t.Setenv("EVENT_PUBLISHER", "")
	t.Setenv("PATH_CATALOGUE_SOURCE", "")
	if _, err := loadBootConfig(); err != nil {
		t.Fatalf("unset EVENT_PUBLISHER / PATH_CATALOGUE_SOURCE mean kafka, got %v", err)
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
		{"log publisher", "EVENT_PUBLISHER", "log", "EVENT_PUBLISHER=log is not supported"},
		{"unknown publisher", "EVENT_PUBLISHER", "rabbit", `unknown EVENT_PUBLISHER "rabbit"`},
		{"file catalogue", "PATH_CATALOGUE_SOURCE", "file", "PATH_CATALOGUE_SOURCE=file is not supported"},
		{"agent url unset", "A2A_AGENT_URL", "", "A2A_AGENT_URL is required: the Agent Card must advertise"},
		{"agent url blank", "A2A_AGENT_URL", "   ", "A2A_AGENT_URL is required"},
		{"agent url relative", "A2A_AGENT_URL", "/a2a", `A2A_AGENT_URL="/a2a" is not an absolute http(s) URL`},
		{"agent url schemeless", "A2A_AGENT_URL", "workforce-management-a2a:8093", "is not an absolute http(s) URL"},
		{"agent url wrong scheme", "A2A_AGENT_URL", "ftp://host/", "is not an absolute http(s) URL"},
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

// run fails fast on a bad configuration: before telemetry, any connection and
// before the listener binds.
func TestRunFailsFastBeforeBindingTheListener(t *testing.T) {
	setBootEnv(t)
	t.Setenv("KAFKA_BROKERS", "")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	t.Setenv("A2A_ADDR", addr)

	start := time.Now()
	if err := run(); err == nil || !strings.Contains(err.Error(), "KAFKA_BROKERS is required") {
		t.Fatalf("run = %v, want a boot error naming KAFKA_BROKERS", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a configuration error must not wait on any dependency")
	}
	if c, derr := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", addr); derr == nil {
		_ = c.Close()
		t.Fatal("the listener was bound although the configuration is invalid")
	}
}
