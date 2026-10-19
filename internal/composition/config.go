package composition

import (
	"errors"
	"fmt"
	"strings"
)

// Boot errors of the mandatory configuration, shared by every composition
// root. Each names its variable (ADR-0038).
var (
	// ErrNoDatabase is the boot error of a missing DATABASE_URL.
	ErrNoDatabase = errors.New("DATABASE_URL is required: this context runs on real Postgres")
	// ErrNoBrokers is the boot error of a missing or blank KAFKA_BROKERS.
	ErrNoBrokers = errors.New("KAFKA_BROKERS is required: this context runs on real Kafka")
)

// ParseBrokers splits a KAFKA_BROKERS value (comma separated), trimming
// blanks; nil when nothing usable is left.
func ParseBrokers(raw string) []string {
	var brokers []string
	for _, b := range strings.Split(raw, ",") {
		if b = strings.TrimSpace(b); b != "" {
			brokers = append(brokers, b)
		}
	}
	return brokers
}

// CheckEventPublisher validates EVENT_PUBLISHER. The only publisher is Kafka
// through the outbox: unset and "kafka" are accepted (the variable is kept
// because the chart and warehouse-infra set it), "log" and anything else is a
// boot error naming the variable.
func CheckEventPublisher(raw string) error {
	if raw == "" || raw == "kafka" {
		return nil
	}
	if raw == "log" {
		return errors.New("EVENT_PUBLISHER=log is not supported: this context publishes to real Kafka")
	}
	return fmt.Errorf("unknown EVENT_PUBLISHER %q: this context publishes to real Kafka (want kafka)", raw)
}

// CheckCatalogueSource validates PATH_CATALOGUE_SOURCE. The process-path
// catalogue is replayed from Kafka: unset and "kafka" are accepted, "file"
// (the retired boot-time YAML source) and anything else is a boot error
// naming the variable.
func CheckCatalogueSource(raw string) error {
	if raw == "" || raw == "kafka" {
		return nil
	}
	if raw == "file" {
		return errors.New("PATH_CATALOGUE_SOURCE=file is not supported: this context runs on real Kafka " +
			"(the process-path catalogue is replayed from warehouse.process-path-management.events)")
	}
	return fmt.Errorf("unknown PATH_CATALOGUE_SOURCE %q: this context runs on real Kafka (want kafka)", raw)
}
