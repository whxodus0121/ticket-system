package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"ticket-system/handler"
	"ticket-system/metrics"
	"ticket-system/repository"
	"ticket-system/service"
	"ticket-system/worker"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func main() {
	// 1. 인프라 설정 (Redis & MySQL)
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:16379",
	})

	ctx := context.Background()
	stockKey := "ticket_stock:concert_2026"

	// 서버 재시작이 기존 재고와 구매자를 지우지 않도록 최초 실행에서만
	// 재고를 초기화합니다.
	if err := rdb.SetNX(ctx, stockKey, 1000, 0).Err(); err != nil {
		log.Fatal("Redis 재고 초기화 실패: ", err)
	}
	currentStock, err := rdb.Get(ctx, stockKey).Int()
	if err != nil {
		log.Fatal("Redis 재고 조회 실패: ", err)
	}
	metrics.TicketStockLevel.Set(float64(currentStock))

	// 2. MySQL 연결 설정 (docker-compose의 ticket-mysql 사용)
	// 비밀번호와 DB명은 docker-compose.yml 설정과 동일하게 유지
	dsn := "root:password123@tcp(127.0.0.1:3306)/ticket_db?charset=utf8mb4&parseTime=True&loc=Local"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB 연결 실패: ", err)
	}

	// DB 커넥션 풀 설정
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("커넥션 풀 설정 실패: ", err)
	}
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetMaxIdleConns(50)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// 3. Repository 생성
	redisRepo := &repository.RedisRepository{Client: rdb}
	mysqlRepo := &repository.MySQLRepository{DB: db}

	// Kafka Repository 생성 (Producer 역할)
	kafkaRepo := repository.NewKafkaRepository([]string{"localhost:9092"}, "ticket-topic")

	// 4. Service 조립 (오류 해결: kafkaRepo 추가)
	svc := service.NewTicketService(redisRepo, mysqlRepo, kafkaRepo)

	// Source topic 소비는 cmd/worker 프로세스 한 곳에서만 수행합니다.
	// API와 별도 worker를 함께 실행했을 때 서로 다른 consumer group으로
	// 같은 이벤트를 중복 처리하던 기존 구성을 제거했습니다.
	recoveryWorker := worker.NewPurchaseWorker(
		[]string{"localhost:9092"},
		"ticket-topic",
		"ticket-group",
		mysqlRepo,
		redisRepo,
		kafkaRepo,
	)
	go svc.StartPromoter(context.Background(), 100) // 100명까지 동시 예매 허용
	go func() {
		for {
			time.Sleep(500 * time.Millisecond) // 1초마다 Redis 실제 값 확인
			val, err := rdb.Get(context.Background(), stockKey).Int()
			if err == nil {
				// Redis의 진짜 값이 0보다 작으면(동시성 이슈 등) 0으로, 아니면 실제 값 그대로 세팅
				if val < 0 {
					metrics.TicketStockLevel.Set(0)
				} else {
					metrics.TicketStockLevel.Set(float64(val))
				}
			}
		}
	}()

	go func() {
		log.Println("📊 Prometheus metrics server started on :8081")
		if err := http.ListenAndServe(":8081", promhttp.Handler()); err != nil {
			log.Printf("메트릭 서버 실행 실패: %v", err)
		}
	}()

	// 6. Handler 조립
	h := handler.NewTicketHandler(svc)

	// 7. 서버 설정 및 경로 등록
	mux := http.NewServeMux()
	mux.Handle("/ticket", h)

	// 취소 핸들러 등록
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error": "user_id가 필요합니다"}`)
			return
		}

		success, message := svc.CancelTicket(r.Context(), userID)
		if !success {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error": "%s"}`, message)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"message": "%s"}`, message)
	})

	mux.HandleFunc("/admin/recover-dlq", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			if err := recoveryWorker.ProcessDLQ(context.Background()); err != nil {
				log.Printf("DLQ 복구 중단: %v", err)
			}
		}()
		fmt.Fprint(w, `{"message": "DLQ 복구 프로세스가 시작되었습니다."}`)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	// 8. 서버 실행 설정
	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Println("🚀 비동기 티켓 시스템 서버 시작 (:8080)...")
	log.Println("- 예매: /ticket")
	log.Println("- 취소: /cancel")

	if err := server.ListenAndServe(); err != nil {
		log.Fatal("서버 시작 실패: ", err)
	}
}
