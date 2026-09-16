package kafkaconnect

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A Kafka message value reaches the published event as the producer wrote it.
// cleat#1641.
//
// kafkaRecord.Key and .Value were `any`, so json.Unmarshal turned every number
// the message carried into a float64 -- an order id or a ledger amount above
// 2^53 arrived at the subscribed workflow as a DIFFERENT number, with no error
// and no log. The column type could not have fixed it: the value was already
// wrong in Go, several layers before storage.
//
// The assertion is on BYTES. A decoded comparison recovers every value a test
// author is likely to pick and is blind exactly where this defect lives.
func TestAKafkaValueReachesTheEventUnchanged(t *testing.T) {
	// A 30-digit integer and a decimal with more significant digits than
	// float64 carries. Both survive a raw copy and neither survives a float64.
	const bigValue = `{"order":123456789012345678901234567890,"rate":0.12345678901234567890123}`
	const bigKey = `9007199254740993`

	var proxySrv *httptest.Server
	proxySrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/consumers/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"instance_id": "test-instance",
				"base_uri":    proxySrv.URL + "/consumers/test-instance",
			})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subscription"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/records"):
			// Written as BYTES rather than through a map, so the fixture
			// itself is not degraded before the code under test sees it --
			// encoding a map[string]any here would have made the test pass
			// against the broken code.
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `[{"topic":"test-topic","key":`+bigKey+
				`,"value":`+bigValue+`,"partition":0,"offset":1}]`)
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxySrv.Close()

	p := &Plugin{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		config:     Config{RestProxyURL: proxySrv.URL},
	}
	c := configRow{
		ID: uuid.New(), TenantID: testTenantID, Name: "big-numbers",
		Brokers: "broker:9092", Topic: "test-topic",
		ConsumerGroup: "cleat-consumer", EventType: "test-topic",
	}

	records, err := p.consumeViaRestProxy(context.Background(), c)
	if err != nil {
		t.Fatalf("consumeViaRestProxy: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("UNMEASURED: expected 1 record, got %d", len(records))
	}

	if got := string(records[0].Value); got != bigValue {
		t.Errorf("the message value was rewritten on the way in.\n"+
			"  produced: %s\n  received: %s\n\n"+
			"kafkaRecord.Value must stay raw JSON. cleat#1641.", bigValue, got)
	}
	if got := string(records[0].Key); got != bigKey {
		t.Errorf("the message key was rewritten on the way in.\n"+
			"  produced: %s\n  received: %s", bigKey, got)
	}
}
