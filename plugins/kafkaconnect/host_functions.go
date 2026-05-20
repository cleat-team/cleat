package kafkaconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("kafka-connect: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "produce"}, p.produce); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type produceInput struct {
	ConfigID uuid.UUID          `json:"config_id"`
	Key      string             `json:"key,omitempty"`
	Value    string             `json:"value"`
	Headers  map[string]string  `json:"headers,omitempty"`
}

type produceOutput struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// kafkaRestProxyPayload is the JSON body posted to the Confluent REST Proxy
// /topics/<topic> endpoint.
type kafkaRestProxyPayload struct {
	Records []kafkaRestProxyRecord `json:"records"`
}

type kafkaRestProxyRecord struct {
	Key       interface{}       `json:"key,omitempty"`
	Value     interface{}       `json:"value"`
	Headers   map[string]string `json:"headers,omitempty"`
	Partition int               `json:"partition,omitempty"`
}

// ---- Host functions ----

// produce sends a message to a Kafka topic. It looks up the Kafka config by ID,
// verifies tenant ownership, and POSTs the message to the configured REST proxy
// or logs the message if no REST proxy is configured.
func (p *Plugin) produce(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("kafka-connect: no tenant context")
	}

	var input produceInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("kafka-connect: invalid input: %w", err)
	}
	if input.ConfigID == uuid.Nil {
		return "", fmt.Errorf("kafka-connect: config_id is required")
	}
	if input.Value == "" {
		return "", fmt.Errorf("kafka-connect: value is required")
	}

	// Look up the Kafka config, verifying tenant ownership.
	var brokers, topic string
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT brokers, topic
			FROM kafka_config
			WHERE id = $1 AND tenant_id = $2 AND enabled = true
		`, p.dialect), input.ConfigID, cc.TenantID).Scan(&brokers, &topic)
	if err != nil {
		return "", fmt.Errorf("kafka-connect: config not found or disabled")
	}

	if p.config.RestProxyURL != "" {
		// Send via Confluent REST Proxy.
		output, err := p.produceViaRestProxy(ctx, topic, input)
		if err != nil {
			return "", err
		}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}

	// No REST proxy configured — log the message as a structural stub.
	p.logger.Info("kafka-connect: produce (stub)",
		"config_id", input.ConfigID,
		"topic", topic,
		"brokers", brokers,
		"key", input.Key,
		"value_length", len(input.Value),
		"headers", input.Headers,
	)

	output := produceOutput{Success: true}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// produceViaRestProxy sends a message to a Kafka topic via the Confluent REST
// Proxy API.
func (p *Plugin) produceViaRestProxy(ctx context.Context, topic string, input produceInput) (produceOutput, error) {
	proxyURL := p.config.RestProxyURL + "/topics/" + topic

	record := kafkaRestProxyRecord{
		Key:     nil,
		Value:   input.Value,
		Headers: input.Headers,
	}
	if input.Key != "" {
		record.Key = input.Key
	}

	payload := kafkaRestProxyPayload{
		Records: []kafkaRestProxyRecord{record},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return produceOutput{}, fmt.Errorf("kafka-connect: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", proxyURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return produceOutput{}, fmt.Errorf("kafka-connect: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/vnd.kafka.json.v2+json")
	req.Header.Set("Accept", "application/vnd.kafka.v2+json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return produceOutput{}, fmt.Errorf("kafka-connect: rest proxy request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return produceOutput{}, fmt.Errorf("kafka-connect: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return produceOutput{}, fmt.Errorf("kafka-connect: rest proxy returned %d: %s", resp.StatusCode, string(respBody))
	}

	p.logger.Info("kafka-connect: produced via REST proxy",
		"topic", topic,
		"key", input.Key,
		"status", resp.StatusCode,
	)

	return produceOutput{Success: true}, nil
}
