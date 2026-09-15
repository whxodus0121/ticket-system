package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"ticket-system/metrics"
	"ticket-system/repository"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/segmentio/kafka-go"
)

var mysqlSaveSuccess = promauto.NewCounter(prometheus.CounterOpts{
	Name: "mysql_save_success_total",
	Help: "The total number of successful MySQL saves",
})

// MessageReader is the subset of kafka.Reader needed for manual offset
// management. A message is never committed merely because it was fetched.
type MessageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, messages ...kafka.Message) error
	Close() error
}

type PurchaseWorker struct {
	Reader        MessageReader
	TicketRepo    repository.TicketRepository
	LockRepo      repository.LockRepository
	KafkaRepo     repository.EventPublisher
	RetryCount    int
	RetryDelay    time.Duration
	brokers       []string
	recoveryGroup string
}

func NewPurchaseWorker(
	brokers []string,
	topic string,
	groupID string,
	tr repository.TicketRepository,
	lr repository.LockRepository,
	kr repository.EventPublisher,
) *PurchaseWorker {
	return &PurchaseWorker{
		Reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:  brokers,
			Topic:    topic,
			GroupID:  groupID,
			MinBytes: 1,
			MaxBytes: 10e6,
			MaxWait:  500 * time.Millisecond,
		}),
		TicketRepo:    tr,
		LockRepo:      lr,
		KafkaRepo:     kr,
		RetryCount:    3,
		RetryDelay:    2 * time.Second,
		brokers:       append([]string(nil), brokers...),
		recoveryGroup: "recovery-group-v2",
	}
}

func (w *PurchaseWorker) Start() {
	defer w.Reader.Close()
	if err := w.Run(context.Background()); err != nil {
		log.Printf("consumer worker stopped without committing the failed message: %v", err)
	}
}

func (w *PurchaseWorker) Run(ctx context.Context) error {
	log.Println("Kafka consumer worker started (manual offset commit)")
	for {
		if err := w.ProcessOne(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Stopping is intentional: fetching a later message and committing it
			// could advance the partition past this uncommitted failure.
			return err
		}
	}
}

// ProcessOne implements the commit boundary:
// DB success -> commit, or DB failure + DLQ success -> commit.
// DLQ failure returns without committing the source message.
func (w *PurchaseWorker) ProcessOne(ctx context.Context) error {
	message, err := w.Reader.FetchMessage(ctx)
	if err != nil {
		return fmt.Errorf("fetch message: %w", err)
	}

	processErr := w.processMessage(ctx, message)
	if processErr != nil {
		dlqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		dlqErr := w.KafkaRepo.PublishToDLQ(dlqCtx, message, processErr.Error())
		cancel()
		if dlqErr != nil {
			return fmt.Errorf("process source offset %d: %v; publish DLQ: %w", message.Offset, processErr, dlqErr)
		}
		log.Printf("message moved to DLQ (partition=%d offset=%d): %v", message.Partition, message.Offset, processErr)
	}

	commitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := w.Reader.CommitMessages(commitCtx, message); err != nil {
		return fmt.Errorf("commit partition %d offset %d: %w", message.Partition, message.Offset, err)
	}
	return nil
}

func (w *PurchaseWorker) processMessage(ctx context.Context, message kafka.Message) error {
	userID := string(message.Key)
	value := string(message.Value)
	if strings.HasPrefix(value, "CANCEL:") {
		return w.handleCancel(ctx, userID, strings.TrimPrefix(value, "CANCEL:"))
	}
	return w.handleSave(userID, value)
}

func (w *PurchaseWorker) handleSave(userID, ticketName string) error {
	var lastErr error
	for attempt := 1; attempt <= w.retryCount(); attempt++ {
		saved, err := w.TicketRepo.SavePurchase(userID, ticketName)
		if err == nil {
			if saved {
				mysqlSaveSuccess.Inc()
				log.Printf("purchase saved: user=%s ticket=%s", userID, ticketName)
			} else {
				log.Printf("duplicate purchase skipped: user=%s ticket=%s", userID, ticketName)
			}
			return nil
		}

		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return nil
		}
		lastErr = err
		if attempt < w.retryCount() {
			time.Sleep(w.RetryDelay)
		}
	}
	return fmt.Errorf("save purchase after %d attempts: %w", w.retryCount(), lastErr)
}

func (w *PurchaseWorker) handleCancel(ctx context.Context, userID, ticketName string) error {
	var lastErr error
	for attempt := 1; attempt <= w.retryCount(); attempt++ {
		if err := w.TicketRepo.DeletePurchase(userID, ticketName); err == nil {
			stock, err := w.LockRepo.FinalizeCancel(ctx, ticketName, userID)
			if err != nil {
				return fmt.Errorf("finalize Redis cancel: %w", err)
			}
			metrics.TicketStockLevel.Set(float64(stock))
			log.Printf("purchase cancelled: user=%s ticket=%s", userID, ticketName)
			return nil
		} else {
			lastErr = err
		}
		if attempt < w.retryCount() {
			time.Sleep(w.RetryDelay)
		}
	}
	return fmt.Errorf("delete purchase after %d attempts: %w", w.retryCount(), lastErr)
}

func (w *PurchaseWorker) retryCount() int {
	if w.RetryCount <= 0 {
		return 1
	}
	return w.RetryCount
}

// ProcessDLQ replays DLQ messages with manual commits. Failed replay messages
// remain uncommitted rather than being recursively republished to the same DLQ.
func (w *PurchaseWorker) ProcessDLQ(ctx context.Context) error {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  w.brokers,
		Topic:    "ticket-dlq-topic",
		GroupID:  w.recoveryGroup,
		MinBytes: 1,
		MaxBytes: 10e6,
		MaxWait:  500 * time.Millisecond,
	})
	defer reader.Close()

	for {
		fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		message, err := reader.FetchMessage(fetchCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil
			}
			return err
		}
		if err := w.processMessage(ctx, message); err != nil {
			return fmt.Errorf("replay DLQ offset %d without commit: %w", message.Offset, err)
		}
		if err := reader.CommitMessages(ctx, message); err != nil {
			return fmt.Errorf("commit DLQ offset %d: %w", message.Offset, err)
		}
	}
}
