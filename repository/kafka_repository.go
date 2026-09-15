package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

type KafkaRepository struct {
	Writer       *kafka.Writer
	Brokers      []string
	DefaultTopic string
}

func NewKafkaRepository(brokers []string, topic string) *KafkaRepository {
	brokerCopy := append([]string(nil), brokers...)
	return &KafkaRepository{
		Writer: &kafka.Writer{
			Addr:     kafka.TCP(brokerCopy...),
			Balancer: &kafka.Hash{},
		},
		Brokers:      brokerCopy,
		DefaultTopic: topic,
	}
}

func (r *KafkaRepository) PublishPurchase(userID, ticketName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return r.Writer.WriteMessages(ctx,
		kafka.Message{
			Topic: r.DefaultTopic,
			Key:   []byte(userID),
			Value: []byte(ticketName),
		},
	)
}

func (r *KafkaRepository) PublishCancel(userID string, ticketName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return r.Writer.WriteMessages(ctx, kafka.Message{
		Topic: r.DefaultTopic,
		Key:   []byte(userID),
		Value: []byte("CANCEL:" + ticketName), // Value에 CANCEL 접두사를 붙여 구분
	})
}

func (r *KafkaRepository) PublishToDLQ(ctx context.Context, source kafka.Message, reason string) error {
	return r.Writer.WriteMessages(ctx, kafka.Message{
		Topic: "ticket-dlq-topic",
		Key:   source.Key,
		Value: source.Value,
		Headers: []kafka.Header{
			{Key: "error_reason", Value: []byte(reason)},
			{Key: "source_topic", Value: []byte(source.Topic)},
			{Key: "source_partition", Value: []byte(fmt.Sprintf("%d", source.Partition))},
			{Key: "source_offset", Value: []byte(fmt.Sprintf("%d", source.Offset))},
		},
	})
}

// PublishToTopic: 특정 토픽으로 메시지를 발행합니다 (DLQ 전송 등에 사용)
func (r *KafkaRepository) PublishToTopic(ctx context.Context, topic string, key, value []byte) error {
	return r.Writer.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   key,
		Value: value,
	})
}

func (r *KafkaRepository) Close() error {
	return r.Writer.Close()
}
