package main

import (
	"errors"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigAcceptsACompleteEnvironment(t *testing.T) {
	c, err := loadConfig(envMap(map[string]string{
		"ANALYTICS_DATABASE_URL": "postgres://u@h/db", "KAFKA_BROKERS": "k1:9092, k2:9092,,", "ANALYTICS_CONSUMER_GROUP": "g",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.analyticsURL != "postgres://u@h/db" || len(c.brokers) != 2 || c.group != "g" {
		t.Fatalf("config = %+v", c)
	}
}

// Each missing value is a boot error naming its variable; the projector has no
// default broker and no default consumer group (ADR-0038).
func TestLoadConfigRefusesEachMissingValue(t *testing.T) {
	full := map[string]string{"ANALYTICS_DATABASE_URL": "postgres://u@h/db", "KAFKA_BROKERS": "k:9092", "ANALYTICS_CONSUMER_GROUP": "g"}
	cases := []struct {
		variable, value string
		want            error
	}{
		{"ANALYTICS_DATABASE_URL", "", errMissingAnalyticsURL},
		{"KAFKA_BROKERS", "", errMissingBrokers},
		{"KAFKA_BROKERS", " , ", errMissingBrokers},
		{"ANALYTICS_CONSUMER_GROUP", "", errMissingGroup},
	}
	for _, tc := range cases {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env[tc.variable] = tc.value
		_, err := loadConfig(envMap(env))
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.variable+" is required") {
			t.Errorf("%s=%q: err = %v, want %v naming the variable", tc.variable, tc.value, err, tc.want)
		}
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	_, err := loadConfig(envMap(nil))
	for _, want := range []error{errMissingAnalyticsURL, errMissingBrokers, errMissingGroup} {
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want it to include %v", err, want)
		}
	}
}

// run fails fast on a missing mandatory variable, before any connection.
func TestRunRefusesToBootWithoutTheConsumerGroup(t *testing.T) {
	t.Setenv("ANALYTICS_DATABASE_URL", "postgres://u@127.0.0.1:1/db")
	t.Setenv("KAFKA_BROKERS", "127.0.0.1:1")
	t.Setenv("ANALYTICS_CONSUMER_GROUP", "")
	if err := run(); !errors.Is(err, errMissingGroup) {
		t.Fatalf("run = %v, want the missing-group boot error", err)
	}
}
