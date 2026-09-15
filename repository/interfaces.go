package repository

import (
	"context"

	"github.com/segmentio/kafka-go"
)

/*
 * LockRepository Interface
 * Redis를 기반으로 고성능 분산 락, 원자적 재고 관리, 대기열 로직을 담당합니다.
 */

type LockRepository interface {
	// Stock and purchase state. RedisRepository implements the multi-key state
	// changes atomically with Lua scripts.
	GetStock(ctx context.Context, ticketName string) (int, error)
	ReservePurchase(ctx context.Context, ticketName, userID string) (string, int, error)
	RollbackPurchase(ctx context.Context, ticketName, userID string) (int, error)
	BeginCancel(ctx context.Context, ticketName, userID string) (string, error)
	AbortCancel(ctx context.Context, ticketName, userID string) error
	FinalizeCancel(ctx context.Context, ticketName, userID string) (int, error)

	// Virtual Waiting Queue
	TryEnterOrEnqueue(ctx context.Context, userID string, maxActive int) (string, int, error)
	RemoveActiveUser(ctx context.Context, userID string) error
	PromoteUsers(ctx context.Context, maxActive int) (int, error)
}

/*
 * TicketRepository Interface
 * 최종적인 티켓 데이터 및 구매 이벤트를 RDBMS(MySQL)에 저장하는 역할을 담당합니다.
 */

type TicketRepository interface {
	GetStock(name string) (int, error)
	DecreaseStock(name string) error
	SavePurchase(userID string, ticketName string) (bool, error)   // 구매 목록 저장
	ExistsPurchase(userID string, ticketName string) (bool, error) //구매 여부 확인
	DeletePurchase(userID string, ticketName string) error
}

// EventPublisher is the Kafka contract used by the service and worker. The
// interface keeps publish/commit failure paths independently testable.
type EventPublisher interface {
	PublishPurchase(userID, ticketName string) error
	PublishCancel(userID, ticketName string) error
	PublishToDLQ(ctx context.Context, message kafka.Message, reason string) error
}
