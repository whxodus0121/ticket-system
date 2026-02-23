package repository

import (
	"context"

	"github.com/segmentio/kafka-go"
)

type KafkaRepository struct {
	Writer *kafka.Writer
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

func (r *KafkaRepository) PublishToTopic(ctx context.Context, topic string, key, value []byte) error {
	return r.Writer.WriteMessages(ctx,
		kafka.Message{
			Topic: topic, // 여기서 토픽을 지정하면 Writer 생성 시의 기본 토픽을 덮어씁니다.
			Key:   key,
			Value: value,
		},
	)
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
