package kafkaconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/plugins/eventtriggers"
)

// Run starts the consumer polling loop. It runs every 5 seconds, reading
// messages from all enabled Kafka configs and publishing them as events
// through the event-triggers system. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("kafka-connect: no database, consumer loop disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	p.logger.Info("kafka-connect: consumer loop started, interval=5s")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("kafka-connect: consumer loop stopped")
			return nil

		case <-ticker.C:
			if err := p.pollConfigs(ctx); err != nil {
				p.logger.Error("kafka-connect: poll configs failed", "error", err)
			}
		}
	}
}

// configRow represents a Kafka configuration fetched from the database.
type configRow struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Name          string
	Brokers       string
	Topic         string
	ConsumerGroup string
	EventType     string
}

// pollConfigs queries all enabled Kafka configs and attempts to consume
// messages from each one, publishing them as events through the
// event-triggers pipeline.
func (p *Plugin) pollConfigs(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, brokers, topic, consumer_group, COALESCE(event_type, topic)
		FROM kafka_config
		WHERE enabled = true
		ORDER BY tenant_id, created_at DESC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var c configRow
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Brokers, &c.Topic, &c.ConsumerGroup, &c.EventType); err != nil {
			p.logger.Error("kafka-connect: scan config row", "error", err)
			continue
		}
		// If no event_type was set, default to the topic name.
		if c.EventType == "" {
			c.EventType = c.Topic
		}

		p.pollConfig(ctx, c)
	}

	return rows.Err()
}

// pollConfig attempts to consume messages from a single Kafka config and
// publish them as events through the event-triggers pipeline.
func (p *Plugin) pollConfig(ctx context.Context, c configRow) {
	if p.config.RestProxyURL == "" {
		p.logger.Debug("kafka-connect: no REST Proxy URL configured, skipping consume",
			"config_id", c.ID,
			"topic", c.Topic,
			"event_type", c.EventType,
		)
		return
	}

	// Consume messages via the Confluent REST Proxy consumer API.
	records, err := p.consumeViaRestProxy(ctx, c)
	if err != nil {
		p.logger.Warn("kafka-connect: consume failed",
			"config_id", c.ID,
			"topic", c.Topic,
			"error", err,
		)
		return
	}

	for _, record := range records {
		if err := p.publishRecord(ctx, c, record); err != nil {
			p.logger.Error("kafka-connect: publish record failed",
				"config_id", c.ID,
				"topic", c.Topic,
				"error", err,
			)
		}
	}
}

// kafkaRecord represents a single Kafka message consumed via the REST Proxy.
type kafkaRecord struct {
	Topic     string                 `json:"topic"`
	Key       interface{}            `json:"key"`
	Value     interface{}            `json:"value"`
	Partition int                    `json:"partition"`
	Offset    int64                  `json:"offset"`
}

// consumeViaRestProxy uses the Confluent REST Proxy v2 consumer API to poll
// for messages from the configured topic. It creates a temporary consumer
// instance, subscribes, polls once, and closes.
func (p *Plugin) consumeViaRestProxy(ctx context.Context, c configRow) ([]kafkaRecord, error) {
	proxyBase := p.config.RestProxyURL

	// Step 1: Create a temporary consumer instance.
	instanceID, baseURI, err := p.createConsumer(ctx, proxyBase, c)
	if err != nil {
		return nil, fmt.Errorf("create consumer: %w", err)
	}
	defer func() {
		// Best-effort cleanup of the consumer instance.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(cleanupCtx, "DELETE", baseURI, nil)
		resp, reqErr := p.httpClient.Do(req)
		if reqErr == nil {
			resp.Body.Close()
		}
	}()

	// Step 2: Subscribe to the topic.
	if err := p.subscribeConsumer(ctx, baseURI, c.Topic); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	p.logger.Debug("kafka-connect: consumer created",
		"config_id", c.ID,
		"topic", c.Topic,
		"instance_id", instanceID,
	)

	// Step 3: Poll for records.
	records, err := p.pollRecords(ctx, baseURI)
	if err != nil {
		return nil, fmt.Errorf("poll records: %w", err)
	}

	return records, nil
}

// createConsumer creates a new consumer instance via the REST Proxy API.
// Returns the instance ID and the base URI for subsequent operations.
func (p *Plugin) createConsumer(ctx context.Context, proxyURL string, c configRow) (string, string, error) {
	consumerURL := proxyURL + "/consumers/" + c.ConsumerGroup

	body := map[string]interface{}{
		"name":                      "cleat-ingest-" + c.ID.String() + "-" + fmt.Sprintf("%d", time.Now().UnixMilli()),
		"format":                    "json",
		"auto.offset.reset":         "latest",
		"auto.commit.enable":        "false",
		"fetch.min.bytes":           "1",
		"consumer.request.timeout.ms": "5000",
	}

	payloadBytes, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", consumerURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/vnd.kafka.v2+json")
	req.Header.Set("Accept", "application/vnd.kafka.v2+json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("create consumer request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("create consumer returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		InstanceID string `json:"instance_id"`
		BaseURI    string `json:"base_uri"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("parse response: %w", err)
	}

	return result.InstanceID, result.BaseURI, nil
}

// subscribeConsumer subscribes the consumer instance to the given topic.
func (p *Plugin) subscribeConsumer(ctx context.Context, baseURI, topic string) error {
	subURL := baseURI + "/subscription"

	body := map[string]interface{}{
		"topics": []string{topic},
	}
	payloadBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", subURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.kafka.v2+json")
	req.Header.Set("Accept", "application/vnd.kafka.v2+json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("subscribe returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// pollRecords fetches records from the consumer instance.
func (p *Plugin) pollRecords(ctx context.Context, baseURI string) ([]kafkaRecord, error) {
	recordsURL := baseURI + "/records?timeout=1000&max_bytes=100000"

	req, err := http.NewRequestWithContext(ctx, "GET", recordsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.kafka.json.v2+json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll request: %w", err)
	}
	defer resp.Body.Close()

	// A 204 No Content means no records available — not an error.
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poll returned %d: %s", resp.StatusCode, string(respBody))
	}

	var records []kafkaRecord
	if err := json.Unmarshal(respBody, &records); err != nil {
		return nil, fmt.Errorf("parse records: %w", err)
	}

	return records, nil
}

// publishRecord publishes a single Kafka message as an event through the
// event-triggers pipeline.
func (p *Plugin) publishRecord(ctx context.Context, c configRow, record kafkaRecord) error {
	eventID := uuid.New()

	// Build the event data from the Kafka message.
	eventData := map[string]interface{}{
		"topic":     c.Topic,
		"partition": record.Partition,
		"offset":    record.Offset,
		"key":       record.Key,
		"value":     record.Value,
		"brokers":   c.Brokers,
		"source":    "kafka",
	}

	// Publish through the event-triggers pipeline.
	matched, err := eventtriggers.PublishEvent(ctx, p.db, p.logger, p.env, eventID, c.TenantID, c.EventType, eventData)
	if err != nil {
		return fmt.Errorf("publish event: %w", err)
	}

	p.logger.Info("kafka-connect: message consumed and published",
		"config_id", c.ID,
		"topic", c.Topic,
		"event_type", c.EventType,
		"event_id", eventID,
		"partition", record.Partition,
		"offset", record.Offset,
		"workflows_started", matched,
	)

	return nil
}
