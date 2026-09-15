package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"ticket-system/repository"
)

var errForcedCommitFailure = errors.New("forced offset commit failure after DB commit")

type failFirstCommitReader struct {
	*kafka.Reader
	failed bool
}

func (r *failFirstCommitReader) CommitMessages(ctx context.Context, messages ...kafka.Message) error {
	if !r.failed {
		r.failed = true
		return errForcedCommitFailure
	}
	return r.Reader.CommitMessages(ctx, messages...)
}

func integrationMySQL(t *testing.T) *repository.MySQLRepository {
	t.Helper()
	dsn := os.Getenv("MYSQL_INTEGRATION_DSN")
	if dsn == "" {
		t.Skip("set MYSQL_INTEGRATION_DSN to run MySQL idempotency integration tests")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect MySQL: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get MySQL connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return repository.NewMySQLRepository(db)
}

func integrationKafkaBroker(t *testing.T) string {
	t.Helper()
	broker := os.Getenv("KAFKA_INTEGRATION_BROKER")
	if broker == "" {
		t.Skip("set KAFKA_INTEGRATION_BROKER to run Kafka idempotency integration tests")
	}
	return broker
}

func newIntegrationReader(broker, topic, group string) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{broker},
		Topic:    topic,
		GroupID:  group,
		MinBytes: 1,
		MaxBytes: 10e6,
		MaxWait:  100 * time.Millisecond,
	})
}

func publishIntegrationMessage(t *testing.T, broker, topic, userID, value string) {
	t.Helper()
	writer := &kafka.Writer{Addr: kafka.TCP(broker), Topic: topic, Balancer: &kafka.Hash{}}
	defer writer.Close()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		lastErr = writer.WriteMessages(ctx, kafka.Message{Key: []byte(userID), Value: []byte(value)})
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("publish integration message: %v", lastErr)
}

func purchaseCount(t *testing.T, repo *repository.MySQLRepository, userID, ticketName string) int64 {
	t.Helper()
	var count int64
	if err := repo.DB.Model(&repository.Purchase{}).
		Where("user_id = ? AND ticket_name = ?", userID, ticketName).
		Count(&count).Error; err != nil {
		t.Fatalf("count purchases: %v", err)
	}
	return count
}

func assertGroupHasNoRedelivery(t *testing.T, broker, topic, group string) {
	t.Helper()
	reader := newIntegrationReader(broker, topic, group)
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := reader.FetchMessage(ctx)
	if err == nil {
		t.Fatal("message was redelivered after the retry committed its offset")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch after committed retry: %v", err)
	}
}

func TestDBCommitBeforeOffsetCommitRedeliveryIsIdempotentIntegration(t *testing.T) {
	broker := integrationKafkaBroker(t)
	mysqlRepo := integrationMySQL(t)
	nonce := time.Now().UnixNano()

	t.Run("BUY unique row is inserted once", func(t *testing.T) {
		topic := fmt.Sprintf("ticket-idempotent-buy-it-%d", nonce)
		group := fmt.Sprintf("ticket-idempotent-buy-group-%d", nonce)
		userID := fmt.Sprintf("idempotent-buy-user-%d", nonce)
		ticketName := fmt.Sprintf("idempotent-buy-ticket-%d", nonce)
		createWorkerTestTopic(t, broker, topic)
		mysqlRepo.DB.Unscoped().Where("user_id = ? AND ticket_name = ?", userID, ticketName).Delete(&repository.Purchase{})
		t.Cleanup(func() {
			mysqlRepo.DB.Unscoped().Where("user_id = ? AND ticket_name = ?", userID, ticketName).Delete(&repository.Purchase{})
		})
		publishIntegrationMessage(t, broker, topic, userID, ticketName)

		firstKafkaReader := newIntegrationReader(broker, topic, group)
		firstReader := &failFirstCommitReader{Reader: firstKafkaReader}
		firstWorker := &PurchaseWorker{
			Reader: firstReader, TicketRepo: mysqlRepo, LockRepo: &fakeLockRepository{},
			KafkaRepo: &fakePublisher{}, RetryCount: 1,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := firstWorker.ProcessOne(ctx)
		cancel()
		firstKafkaReader.Close()
		if !errors.Is(err, errForcedCommitFailure) && (err == nil || !strings.Contains(err.Error(), errForcedCommitFailure.Error())) {
			t.Fatalf("first processing error=%v, want forced commit failure", err)
		}
		if count := purchaseCount(t, mysqlRepo, userID, ticketName); count != 1 {
			t.Fatalf("rows after DB commit and offset failure=%d, want 1", count)
		}

		secondReader := newIntegrationReader(broker, topic, group)
		secondWorker := &PurchaseWorker{
			Reader: secondReader, TicketRepo: mysqlRepo, LockRepo: &fakeLockRepository{},
			KafkaRepo: &fakePublisher{}, RetryCount: 1,
		}
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		err = secondWorker.ProcessOne(ctx)
		cancel()
		secondReader.Close()
		if err != nil {
			t.Fatalf("redelivered BUY: %v", err)
		}
		if count := purchaseCount(t, mysqlRepo, userID, ticketName); count != 1 {
			t.Fatalf("rows after redelivery=%d, want 1", count)
		}
		assertGroupHasNoRedelivery(t, broker, topic, group)
	})

	t.Run("CANCEL delete and Redis stock release happen once", func(t *testing.T) {
		redisAddress := os.Getenv("REDIS_INTEGRATION_ADDR")
		if redisAddress == "" {
			t.Skip("set REDIS_INTEGRATION_ADDR to run CANCEL idempotency integration test")
		}
		topic := fmt.Sprintf("ticket-idempotent-cancel-it-%d", nonce)
		group := fmt.Sprintf("ticket-idempotent-cancel-group-%d", nonce)
		userID := fmt.Sprintf("idempotent-cancel-user-%d", nonce)
		ticketName := fmt.Sprintf("idempotent-cancel-ticket-%d", nonce)
		createWorkerTestTopic(t, broker, topic)

		redisClient := redis.NewClient(&redis.Options{Addr: redisAddress})
		redisRepo := &repository.RedisRepository{Client: redisClient}
		stockKey := "ticket_stock:" + ticketName
		purchasedKey := "purchased_users:" + ticketName
		pendingKey := "pending_cancels:" + ticketName
		ctx := context.Background()
		if err := redisClient.Set(ctx, stockKey, 0, 0).Err(); err != nil {
			t.Fatalf("seed Redis stock: %v", err)
		}
		if err := redisClient.SAdd(ctx, purchasedKey, userID).Err(); err != nil {
			t.Fatalf("seed Redis purchaser: %v", err)
		}
		if err := redisClient.SAdd(ctx, pendingKey, userID).Err(); err != nil {
			t.Fatalf("seed Redis pending cancel: %v", err)
		}
		mysqlRepo.DB.Unscoped().Where("user_id = ? AND ticket_name = ?", userID, ticketName).Delete(&repository.Purchase{})
		if saved, err := mysqlRepo.SavePurchase(userID, ticketName); err != nil || !saved {
			t.Fatalf("seed MySQL purchase saved=%v err=%v", saved, err)
		}
		t.Cleanup(func() {
			redisClient.Del(ctx, stockKey, purchasedKey, pendingKey)
			redisClient.Close()
			mysqlRepo.DB.Unscoped().Where("user_id = ? AND ticket_name = ?", userID, ticketName).Delete(&repository.Purchase{})
		})
		publishIntegrationMessage(t, broker, topic, userID, "CANCEL:"+ticketName)

		firstKafkaReader := newIntegrationReader(broker, topic, group)
		firstReader := &failFirstCommitReader{Reader: firstKafkaReader}
		firstWorker := &PurchaseWorker{
			Reader: firstReader, TicketRepo: mysqlRepo, LockRepo: redisRepo,
			KafkaRepo: &fakePublisher{}, RetryCount: 1,
		}
		processCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := firstWorker.ProcessOne(processCtx)
		cancel()
		firstKafkaReader.Close()
		if err == nil || !strings.Contains(err.Error(), errForcedCommitFailure.Error()) {
			t.Fatalf("first processing error=%v, want forced commit failure", err)
		}
		if count := purchaseCount(t, mysqlRepo, userID, ticketName); count != 0 {
			t.Fatalf("rows after CANCEL DB commit=%d, want 0", count)
		}

		secondReader := newIntegrationReader(broker, topic, group)
		secondWorker := &PurchaseWorker{
			Reader: secondReader, TicketRepo: mysqlRepo, LockRepo: redisRepo,
			KafkaRepo: &fakePublisher{}, RetryCount: 1,
		}
		processCtx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		err = secondWorker.ProcessOne(processCtx)
		cancel()
		secondReader.Close()
		if err != nil {
			t.Fatalf("redelivered CANCEL: %v", err)
		}
		stock, err := redisClient.Get(ctx, stockKey).Int()
		if err != nil {
			t.Fatalf("read Redis stock: %v", err)
		}
		purchasers, _ := redisClient.SCard(ctx, purchasedKey).Result()
		pending, _ := redisClient.SCard(ctx, pendingKey).Result()
		if count := purchaseCount(t, mysqlRepo, userID, ticketName); count != 0 || stock != 1 || purchasers != 0 || pending != 0 {
			t.Fatalf("rows=%d stock=%d purchasers=%d pending=%d, want 0/1/0/0", count, stock, purchasers, pending)
		}
		assertGroupHasNoRedelivery(t, broker, topic, group)
	})
}
