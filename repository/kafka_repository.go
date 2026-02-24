package repository

import (
	"context"

	"github.com/segmentio/kafka-go"
)

type KafkaRepository struct {
	Writer  *kafka.Writer
	Brokers []string
}

func NewKafkaRepository(brokers []string, topic string) *KafkaRepository {
	return &KafkaRepository{
		Writer: &kafka.Writer{
			Addr: kafka.TCP(brokers...),
			//Topic:    topic,
			Balancer: &kafka.LeastBytes{},
		},
	}
}

func (r *KafkaRepository) PublishPurchase(userID, ticketName string) error {
	return r.Writer.WriteMessages(context.Background(),
		kafka.Message{
			Topic: "ticket-topic",
			Key:   []byte(userID),
			Value: []byte(ticketName),
		},
	)
}

func (r *KafkaRepository) PublishCancel(userID string, ticketName string) error {
	return r.Writer.WriteMessages(context.Background(), kafka.Message{
		Topic: "ticket-topic",
		Key:   []byte(userID),
		Value: []byte("CANCEL:" + ticketName), // Value에 CANCEL 접두사를 붙여 구분
	})
}

func (r *KafkaRepository) PublishToDLQ(ctx context.Context, key, value []byte, reason string) error {
	return r.Writer.WriteMessages(ctx,
		kafka.Message{
			Topic: "ticket-dlq-topic",
			Key:   key,
			Value: value,
			Headers: []kafka.Header{
				{Key: "error_reason", Value: []byte(reason)},
			},
		},
	)
}

// PublishToTopic: 특정 토픽으로 메시지를 발행합니다 (DLQ 전송 등에 사용)
func (r *KafkaRepository) PublishToTopic(ctx context.Context, topic string, key, value []byte) error {
	writer := &kafka.Writer{
		Addr:     kafka.TCP(r.Brokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
	}
	defer writer.Close()

	return writer.WriteMessages(ctx, kafka.Message{
		Key:   key,
		Value: value,
	})
}
