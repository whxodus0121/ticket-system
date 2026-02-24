package main

import (
	"log"
	"net/http"
	"ticket-system/repository"
	"ticket-system/worker"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

/*
 * Purchase Consumer Worker
 * Kafka로부터 구매 이벤트를 소비하여 MySQL에 최종적으로 데이터를 영속화하는 역할을 수행합니다.
 */

func main() {
	// 1. Database Connection (GORM)
	dsn := "root:password123@tcp(127.0.0.1:3306)/ticket_db?charset=utf8mb4&parseTime=True&loc=Local"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("DB 연결 실패: %v", err)
	}

	// 2. Repository 초기화 (Dependency Injection)
	ticketRepo := repository.NewMySQLRepository(db)
	kafkaRepo := repository.NewKafkaRepository([]string{"localhost:9092"}, "ticket-topic")

	// 3. Prometheus Metrics Server (Monitoring)
	// 독립적인 고루틴에서 메트릭 서버를 실행하여 메인 로직과 분리합니다.
	go func() {
		log.Println("📊 Prometheus 메트릭 서버 시작 중... (:8081/metrics)")
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":8081", nil); err != nil {
			log.Fatalf("메트릭 서버 실행 실패: %v", err)
		}
	}()

	// 4. Purchase Worker 실행
	// 비동기 쓰기 작업을 통해 트래픽 병목을 방지하고 최종 일관성을 보장합니다.
	pWorker := worker.NewPurchaseWorker(
		[]string{"localhost:9092"},
		"ticket-topic",
		"ticket-group",
		ticketRepo,
		kafkaRepo,
	)

	pWorker.Start()
}
