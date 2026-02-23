package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"ticket-system/repository"
	"time" // 재시도 대기를 위해 추가

	"github.com/go-sql-driver/mysql"
	"github.com/segmentio/kafka-go"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// DB 저장 성공 횟수를 기록하는 카운터
	mysqlSaveSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mysql_save_success_total",
		Help: "The total number of successful MySQL saves",
	})
)

type PurchaseWorker struct {
	Reader     *kafka.Reader
	TicketRepo repository.TicketRepository
	KafkaRepo  *repository.KafkaRepository // DLQ 전송을 위한 레포지토리 추가
}

func NewPurchaseWorker(brokers []string, topic string, groupID string, tr repository.TicketRepository, kr *repository.KafkaRepository) *PurchaseWorker {
	return &PurchaseWorker{
		Reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:  brokers,
			Topic:    topic,
			GroupID:  groupID,
			MinBytes: 10e3,
			MaxBytes: 10e6,
		}),
		TicketRepo: tr,
		KafkaRepo:  kr,
	}
}

func (w *PurchaseWorker) Start() {
	fmt.Println("🚀 Kafka Consumer Worker 시작... [예매 저장/취소 처리 대기 중]")

	for {
		m, err := w.Reader.ReadMessage(context.Background())
		if err != nil {
			log.Printf("❌ 메시지 읽기 에러: %v", err)
			continue
		}

		userID := string(m.Key)
		messageVal := string(m.Value)

		if strings.HasPrefix(messageVal, "CANCEL:") {
			ticketName := strings.TrimPrefix(messageVal, "CANCEL:")

			w.handleCancel(userID, ticketName, m)
		} else {

			w.handleSave(userID, messageVal, m)
		}
	}
}

func (w *PurchaseWorker) handleSave(userID string, ticketName string, rawMsg kafka.Message) {

	time.Sleep(100 * time.Millisecond)

	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		saved, err := w.TicketRepo.SavePurchase(userID, ticketName)

		if err == nil {
			if !saved {
				log.Printf("⚠️ [중복 저장 스킵] 유저 %s는 이미 처리되었습니다.", userID)
			} else {
				mysqlSaveSuccess.Inc()
				fmt.Printf("✅ [저장 성공] 유저 %s의 티켓 정보 MySQL 저장 완료\n", userID)
			}
			return
		}

		lastErr = err
		var mysqlErr *mysql.MySQLError
		// 중복 키(1062)는 재시도할 필요가 없으므로 즉시 종료
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			log.Printf("⚠️ [중복 저장 스킵] 유저 %s는 이미 처리되었습니다.", userID)
			return
		}

		log.Printf("🚨 [저장 실패] 유저 %s (재시도 %d/%d): %v", userID, i+1, maxRetries, err)
		time.Sleep(time.Second * 2)
	}

	// [수정 포인트] image_6b283의 UnusedVar 에러 해결: 마지막 에러 정보를 로그에 활용
	log.Printf("❌ [최종 실패] 유저 %s 메시지 DLQ 이동. 사유: %v", userID, lastErr)

	// DLQ 전송 시 에러 사유를 포함해서 전송
	err := w.KafkaRepo.PublishToTopic(context.Background(), "ticket-dlq-topic", rawMsg.Key, rawMsg.Value)
	if err != nil {
		log.Printf("💣 [치명적 에러] DLQ 전송 실패: %v", err)
	}
}

func (w *PurchaseWorker) handleCancel(userID string, ticketName string, rawMsg kafka.Message) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		err := w.TicketRepo.DeletePurchase(userID, ticketName)

		if err == nil {
			fmt.Printf("🗑️ [취소 성공] 유저 %s의 구매 내역 DB 삭제 완료\n", userID)
			return // 성공 시 종료
		}

		lastErr = err
		log.Printf("🚨 [취소 실패] 유저 %s (재시도 %d/%d): %v", userID, i+1, maxRetries, err)
		time.Sleep(time.Second * 2) // 2초 대기
	}

	// 3번 모두 실패 시 DLQ로 전송
	log.Printf("❌ [취소 최종 실패] 유저 %s의 취소 메시지 DLQ 이동. 사유: %v", userID, lastErr)

	// DLQ 토픽으로 전송 (예매와 같은 토픽을 써도 되고, ticket-cancel-dlq-topic으로 나눠도 됩니다)
	err := w.KafkaRepo.PublishToTopic(context.Background(), "ticket-dlq-topic", rawMsg.Key, rawMsg.Value)
	if err != nil {
		log.Printf("💣 [치명적 에러] 취소 DLQ 전송 실패: %v", err)
	}
}

func (w *PurchaseWorker) ProcessDLQ() {
	log.Println("🛠️ [DLQ 복구] 저장 실패했던 데이터를 다시 처리합니다...")

	// 복구용 리더 (그룹 ID를 다르게 해서 처음부터 읽음)
	dlqReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     w.Reader.Config().Brokers,
		Topic:       "ticket-dlq-topic",
		GroupID:     "recovery-group-v1",
		StartOffset: kafka.FirstOffset,
	})
	defer dlqReader.Close()

	for {
		// 더 이상 읽을 메시지가 없으면 3초 뒤 종료되도록 타임아웃 설정
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		m, err := dlqReader.ReadMessage(ctx)
		cancel()

		if err != nil {
			log.Println("✅ [DLQ 복구 완료] 모든 유실 데이터를 처리했거나 남은 데이터가 없습니다.")
			return
		}

		userID := string(m.Key)
		messageVal := string(m.Value)

		if strings.HasPrefix(messageVal, "CANCEL:") {
			ticketName := strings.TrimPrefix(messageVal, "CANCEL:")
			log.Printf("🔄 [DLQ 취소 재처리] 유저: %s", userID)
			w.handleCancel(userID, ticketName, m)
		} else {
			log.Printf("🔄 [DLQ 저장 재처리] 유저: %s", userID)
			w.handleSave(userID, messageVal, m)
		}
	}
}
