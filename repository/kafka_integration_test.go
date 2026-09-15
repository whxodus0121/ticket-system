package repository

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func createKafkaTopic(t *testing.T, broker, topic string, partitions int) {
	t.Helper()
	connection, err := kafka.Dial("tcp", broker)
	if err != nil {
		t.Fatalf("dial Kafka: %v", err)
	}
	controller, err := connection.Controller()
	connection.Close()
	if err != nil {
		t.Fatalf("find Kafka controller: %v", err)
	}
	controllerConnection, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, fmt.Sprint(controller.Port)))
	if err != nil {
		t.Fatalf("dial Kafka controller: %v", err)
	}
	defer controllerConnection.Close()
	if err := controllerConnection.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	t.Cleanup(func() {
		connection, err := kafka.Dial("tcp", broker)
		if err != nil {
			return
		}
		controller, err := connection.Controller()
		connection.Close()
		if err != nil {
			return
		}
		controllerConnection, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, fmt.Sprint(controller.Port)))
		if err != nil {
			return
		}
		defer controllerConnection.Close()
		_ = controllerConnection.DeleteTopics(topic)
	})
}

func TestKafkaHashOrderingIntegration(t *testing.T) {
	broker := os.Getenv("KAFKA_INTEGRATION_BROKER")
	if broker == "" {
		t.Skip("set KAFKA_INTEGRATION_BROKER to run Kafka integration tests")
	}
	topic := fmt.Sprintf("ticket-ordering-it-%d", time.Now().UnixNano())
	createKafkaTopic(t, broker, topic, 3)

	publisher := NewKafkaRepository([]string{broker}, topic)
	defer publisher.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := publisher.PublishToTopic(ctx, topic, []byte("same-user"), []byte("BUY")); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishToTopic(ctx, topic, []byte("same-user"), []byte("CANCEL")); err != nil {
		t.Fatal(err)
	}

	foundPartition := -1
	for partition := 0; partition < 3; partition++ {
		connection, err := kafka.DialLeader(ctx, "tcp", broker, topic, partition)
		if err != nil {
			t.Fatal(err)
		}
		first, last, err := connection.ReadOffsets()
		if err != nil {
			connection.Close()
			t.Fatal(err)
		}
		if last-first == 0 {
			connection.Close()
			continue
		}
		if last-first != 2 || foundPartition != -1 {
			connection.Close()
			t.Fatalf("events spread across partitions or unexpected count: partition=%d count=%d", partition, last-first)
		}
		foundPartition = partition
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		firstMessage, err := connection.ReadMessage(1024)
		if err != nil {
			connection.Close()
			t.Fatal(err)
		}
		secondMessage, err := connection.ReadMessage(1024)
		connection.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(firstMessage.Value) != "BUY" || string(secondMessage.Value) != "CANCEL" {
			t.Fatalf("message order=%q,%q", firstMessage.Value, secondMessage.Value)
		}
	}
	if foundPartition == -1 {
		t.Fatal("no partition contained the produced events")
	}
}
