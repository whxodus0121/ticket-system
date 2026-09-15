package main

import (
	"log"
	"net/http"
	"sync"
	"ticket-system/repository"
	"ticket-system/worker"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

/*
 * Purchase Consumer Worker
 * Kafka로부터 구매 이벤트를 소비하여 MySQL에 최종적으로 데이터를 영속화하는 역할을 수행합니다.
 * 3개의 워커 인스턴스를 같은 컨슈머 그룹("ticket-group")으로 띄워서,
 * Kafka 토픽의 파티션 3개를 각각 하나씩 나눠 맡아 병렬로 소비하도록 구성합니다.
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
	redisClient := redis.NewClient(&redis.Options{Addr: "localhost:16379"})
	defer redisClient.Close()
	redisRepo := &repository.RedisRepository{Client: redisClient}

	// 3. Prometheus Metrics Server (Monitoring)
	// 독립적인 고루틴에서 메트릭 서버를 실행하여 메인 로직과 분리합니다.
	go func() {
		log.Println("📊 Prometheus 메트릭 서버 시작 중... (:8082/metrics)")
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":8082", nil); err != nil {
			log.Fatalf("메트릭 서버 실행 실패: %v", err)
		}
	}()

	// 4. Purchase Worker 3개 인스턴스 실행
	// 같은 GroupID("ticket-group")로 3개를 띄우면, Kafka 컨슈머 그룹 리밸런싱에 의해 토픽의 파티션 3개가 이 3개 워커에 하나씩 자동으로 분배됩니다.

	const workerCount = 3
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// 워커별로 독립된 KafkaRepository(Writer)를 사용해 DLQ 전송 시 충돌을 방지합니다.
			kafkaRepo := repository.NewKafkaRepository([]string{"localhost:9092"}, "ticket-topic")
			defer kafkaRepo.Close()

			pWorker := worker.NewPurchaseWorker(
				[]string{"localhost:9092"},
				"ticket-topic",
				"ticket-group",
				ticketRepo,
				redisRepo,
				kafkaRepo,
			)

			log.Printf("🚀 Worker #%d 시작 (GroupID: ticket-group)", workerID)
			pWorker.Start() // 내부에서 무한 루프로 블로킹
		}(i + 1)
	}

	wg.Wait()
}
