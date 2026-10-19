package composition_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/claudioed/workforce-management/internal/composition"
)

func TestParseBrokers(t *testing.T) {
	cases := map[string][]string{
		"":                       nil,
		" , ,":                   nil,
		"a:9092":                 {"a:9092"},
		" a:9092, b:9092 ,,":     {"a:9092", "b:9092"},
		"kafka.svc.cluster:9092": {"kafka.svc.cluster:9092"},
	}
	for raw, want := range cases {
		if got := composition.ParseBrokers(raw); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseBrokers(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestMandatoryErrorsNameTheirVariable(t *testing.T) {
	if !strings.Contains(composition.ErrNoDatabase.Error(), "DATABASE_URL is required") {
		t.Errorf("ErrNoDatabase = %v", composition.ErrNoDatabase)
	}
	if !strings.Contains(composition.ErrNoBrokers.Error(), "KAFKA_BROKERS is required") {
		t.Errorf("ErrNoBrokers = %v", composition.ErrNoBrokers)
	}
	if errors.Is(composition.ErrNoDatabase, composition.ErrNoBrokers) {
		t.Error("the two boot errors must be distinct")
	}
}

// Each accepted or rejected value, with the text the error must carry.
func TestCheckEventPublisher(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"", ""},
		{"kafka", ""},
		{"log", "EVENT_PUBLISHER=log is not supported: this context publishes to real Kafka"},
		{"rabbit", `unknown EVENT_PUBLISHER "rabbit"`},
	}
	for _, tc := range cases {
		assertCheck(t, "EVENT_PUBLISHER", tc.raw, tc.want, composition.CheckEventPublisher(tc.raw))
	}
}

func TestCheckCatalogueSource(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"", ""},
		{"kafka", ""},
		{"file", "PATH_CATALOGUE_SOURCE=file is not supported: this context runs on real Kafka"},
		{"s3", `unknown PATH_CATALOGUE_SOURCE "s3"`},
	}
	for _, tc := range cases {
		assertCheck(t, "PATH_CATALOGUE_SOURCE", tc.raw, tc.want, composition.CheckCatalogueSource(tc.raw))
	}
}

func assertCheck(t *testing.T, variable, raw, want string, err error) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Errorf("%s=%q: unexpected error %v", variable, raw, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s=%q: err = %v, want one containing %q", variable, raw, err, want)
		return
	}
	if !strings.Contains(err.Error(), variable) {
		t.Errorf("%s=%q: the error does not name the variable: %v", variable, raw, err)
	}
}
