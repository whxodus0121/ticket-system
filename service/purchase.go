package service

import (
	"context"
	"ticket-system/metrics"
	"ticket-system/repository"
)

type TicketService struct {
	LockRepo   repository.LockRepository
	TicketRepo repository.TicketRepository
	KafkaRepo  *repository.KafkaRepository
}

func NewTicketService(lr repository.LockRepository, tr repository.TicketRepository, kr *repository.KafkaRepository) *TicketService {
	return &TicketService{LockRepo: lr, TicketRepo: tr, KafkaRepo: kr}
}

// BuyTicket: 대기열 진입부터 예매 성공까지의 핵심 로직
func (s *TicketService) BuyTicket(userID string) (string, int) {
	ctx := context.Background()
	ticketName := "concert_2026"
	maxActive := 100

	// 1. 빠른 재고 확인
	currentStock, err := s.LockRepo.GetStock(ctx, ticketName)
	if err != nil || currentStock <= 0 {
		return "SOLD_OUT", 0
	}

	// 2. 가상 대기열 진입 시도
	status, rank, err := s.LockRepo.TryEnterOrEnqueue(ctx, userID, maxActive)
	if err != nil || status == "WAITING" {
		return status, rank
	}

	// 3. 진입 성공 시, 함수 종료 시점에 무조건 Active Set에서 유저 제거 (defer 사용)
	defer s.LockRepo.RemoveActiveUser(ctx, userID)
	metrics.PurchaseRequests.Inc()

	// 4. 중복 구매 체크
	if purchased, _ := s.LockRepo.IsUserPurchased(ctx, ticketName, userID); purchased {
		return "ALREADY_PURCHASED", 0
	}

	// 5. Redis 재고 차감 (Lua Script 호출)
	remaining, err := s.LockRepo.DecreaseStock(ctx, ticketName)
	if err != nil || remaining < 0 {
		return "SOLD_OUT", 0
	}

	// 6. Kafka로 예매 이벤트 발행 (비동기 저장 시작)
	if err := s.KafkaRepo.PublishPurchase(userID, ticketName); err != nil {
		s.rollbackRedis(ctx, ticketName, userID) // 실패 시 재고 복구
		return "FAIL", 0
	}

	// 7. 구매자 명단 추가
	s.LockRepo.AddPurchasedUser(ctx, ticketName, userID)
	metrics.TicketStockLevel.Set(float64(remaining))

	return "SUCCESS", remaining
}

// CancelTicket: 예매 취소 로직
func (s *TicketService) CancelTicket(userID string) (bool, string) {
	ctx := context.Background()
	ticketName := "concert_2026"

	isPurchased, err := s.LockRepo.IsUserPurchased(ctx, ticketName, userID)
	if err != nil || !isPurchased {
		return false, "구매 내역이 없거나 이미 취소되었습니다."
	}

	newStock, err := s.LockRepo.IncreaseStock(ctx, ticketName)
	if err != nil {
		return false, "재고 복구 중 오류가 발생했습니다."
	}

	metrics.TicketStockLevel.Set(float64(newStock))
	s.LockRepo.RemovePurchasedUser(ctx, ticketName, userID)
	s.KafkaRepo.PublishCancel(userID, ticketName)

	return true, "취소 요청이 접수되었습니다."
}

// rollbackRedis: Kafka 전송 실패 등 예외 상황 발생 시 Redis 재고 원상복구
func (s *TicketService) rollbackRedis(ctx context.Context, ticketName, userID string) {
	newStock, _ := s.LockRepo.IncreaseStock(ctx, ticketName)
	metrics.TicketStockLevel.Set(float64(newStock))
}
