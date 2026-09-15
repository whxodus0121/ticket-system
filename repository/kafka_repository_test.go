package repository

import (
	"reflect"
	"testing"

	"github.com/segmentio/kafka-go"
)

func TestNewKafkaRepositoryKeepsBrokersAndUsesHashBalancer(t *testing.T) {
	brokers := []string{"broker-a:9092", "broker-b:9092"}
	repository := NewKafkaRepository(brokers, "ticket-topic")
	defer repository.Close()

	if !reflect.DeepEqual(repository.Brokers, brokers) {
		t.Fatalf("brokers=%v want=%v", repository.Brokers, brokers)
	}
	if _, ok := repository.Writer.Balancer.(*kafka.Hash); !ok {
		t.Fatalf("balancer=%T, want *kafka.Hash", repository.Writer.Balancer)
	}
}
