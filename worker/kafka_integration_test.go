package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"ticket-system/repository"
)

func createWorkerTestTopic(t *testing.T, broker, topic string) {
	t.Helper()
	connection, err := kafka.Dial("tcp", broker)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := connection.Controller()
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	controllerConnection, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, fmt.Sprint(controller.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer controllerConnection.Close()
	if err := controllerConnection.CreateTopics(kafka.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		leader, err := kafka.DialLeader(ctx, "tcp", broker, topic, 0)
		cancel()
		if err == nil {
			leader.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s did not become ready: %v", topic, err)
		}
		time.Sleep(100 * time.Millisecond)
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

func TestDLQPublishFailureLeavesSourceForRestartIntegration(t *testing.T) {
	broker := os.Getenv("KAFKA_INTEGRATION_BROKER")
	if broker == "" {
		t.Skip("set KAFKA_INTEGRATION_BROKER to run Kafka integration tests")
	}
	topic := fmt.Sprintf("ticket-uncommitted-it-%d", time.Now().UnixNano())
	group := fmt.Sprintf("ticket-uncommitted-group-%d", time.Now().UnixNano())
	createWorkerTestTopic(t, broker, topic)

	publishIntegrationMessage(t, broker, topic, "restart-user", "concert_2026")

	firstReader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{broker}, Topic: topic, GroupID: group, MinBytes: 1, MaxBytes: 10e6, MaxWait: 100 * time.Millisecond})
	badPublisher := repository.NewKafkaRepository([]string{"127.0.0.1:19092"}, topic)
	firstWorker := &PurchaseWorker{
		Reader:     firstReader,
		TicketRepo: &fakeTicketRepository{saveErr: errors.New("db down")},
		LockRepo:   &fakeLockRepository{},
		KafkaRepo:  badPublisher,
		RetryCount: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := firstWorker.ProcessOne(ctx)
	cancel()
	firstReader.Close()
	badPublisher.Close()
	if err == nil {
		t.Fatal("expected real DLQ broker connection failure")
	}

	secondReader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{broker}, Topic: topic, GroupID: group, MinBytes: 1, MaxBytes: 10e6, MaxWait: 100 * time.Millisecond})
	defer secondReader.Close()
	restartCtx, restartCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer restartCancel()
	message, err := secondReader.FetchMessage(restartCtx)
	if err != nil {
		t.Fatalf("uncommitted message was not redelivered: %v", err)
	}
	if string(message.Key) != "restart-user" {
		t.Fatalf("redelivered key=%q", message.Key)
	}
}
