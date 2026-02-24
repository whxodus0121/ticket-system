package repository

import (
	"context"
	"time"
)

/*
 * LockRepository Interface
 * Redis를 기반으로 고성능 분산 락, 원자적 재고 관리, 대기열 로직을 담당합니다.
 */

type LockRepository interface {
	// Stock Management
	GetStock(ctx context.Context, ticketName string) (int, error)
	DecreaseStock(ctx context.Context, ticketName string) (int, error)
	IncreaseStock(ctx context.Context, ticketName string) (int, error)

	// User Verification
	IsUserPurchased(ctx context.Context, ticketName string, userID string) (bool, error)
	AddPurchasedUser(ctx context.Context, ticketName string, userID string) error
	RemovePurchasedUser(ctx context.Context, ticketName string, userID string) error

	// Virtual Waiting Queue
	TryEnterOrEnqueue(ctx context.Context, userID string, maxActive int) (string, int, error)
	RemoveActiveUser(ctx context.Context, userID string) error
	PromoteUsers(ctx context.Context, maxActive int) (int, error)

	// Distributed Locking
	Lock(ctx context.Context, key string, expiration time.Duration) (bool, error)
	Unlock(ctx context.Context, key string) error
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
