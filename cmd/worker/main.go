package main

import (
	"log"
	"net/http"
	"ticket-system/repository"
	"ticket-system/worker"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/driver/mysql" // GORM용 드라이버
	"gorm.io/gorm"
)

func main() {
	// 1. GORM으로 MySQL 연결
	dsn := "root:password123@tcp(127.0.0.1:3306)/ticket_db?charset=utf8mb4&parseTime=True&loc=Local"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("DB 연결 실패: %v", err)
	}

	// 2. 레포지토리 초기화
	ticketRepo := repository.NewMySQLRepository(db)
	kafkaRepo := repository.NewKafkaRepository([]string{"localhost:9092"}, "ticket-topic")

	go func() {
		log.Println("📊 Prometheus 메트릭 서버 시작 중... (:8081/metrics)")
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":8081", nil); err != nil {
			log.Fatalf("메트릭 서버 실행 실패: %v", err)
		}
	}()

	// 3. 워커 생성 및 시작
	pWorker := worker.NewPurchaseWorker(
		[]string{"localhost:9092"},
		"ticket-topic",
		"ticket-group",
		ticketRepo,
		kafkaRepo,
	)

	pWorker.Start()
}
