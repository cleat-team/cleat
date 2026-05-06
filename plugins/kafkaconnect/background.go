package kafkaconnect

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Run starts the consumer polling loop. It runs every 5 seconds, querying all
// enabled kafka_configs and logging a message for each (actual signal delivery
// is a later integration). Returns when ctx is cancelled.
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
}

// pollConfigs queries all enabled Kafka configs and logs a consumer poll
// message for each. In a future iteration, this will actually consume
// messages from Kafka and deliver signals to workflows.
func (p *Plugin) pollConfigs(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, brokers, topic, consumer_group
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
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Brokers, &c.Topic, &c.ConsumerGroup); err != nil {
			p.logger.Error("kafka-connect: scan config row", "error", err)
			continue
		}

		p.pollConfig(ctx, c)
	}

	return rows.Err()
}

// pollConfig logs a consumer poll attempt for a single Kafka config. This is
// a structural stub — actual signal delivery will be implemented in a later
// integration.
func (p *Plugin) pollConfig(ctx context.Context, c configRow) {
	p.logger.Info("kafka-connect: consumer poll (stub)",
		"config_id", c.ID,
		"tenant", c.TenantID,
		"name", c.Name,
		"brokers", c.Brokers,
		"topic", c.Topic,
		"consumer_group", c.ConsumerGroup,
	)
}

