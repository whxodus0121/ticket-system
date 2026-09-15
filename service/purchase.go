package service

import (
	"context"
	"ticket-system/metrics"
	"ticket-system/repository"
)

type TicketService struct {
	LockRepo   repository.LockRepository
	TicketRepo repository.TicketRepository
	KafkaRepo  repository.EventPublisher
}

func NewTicketService(lr repository.LockRepository, tr repository.TicketRepository, kr repository.EventPublisher) *TicketService {
	return &TicketService{LockRepo: lr, TicketRepo: tr, KafkaRepo: kr}
}

// BuyTicket: 대기열 진입부터 예매 성공까지의 핵심 로직
func (s *TicketService) BuyTicket(ctx context.Context, userID string) (string, int) {
	ticketName := "concert_2026"
	maxActive := 100

	// 1. 빠른 재고 확인
	currentStock, err := s.LockRepo.GetStock(ctx, ticketName)
	if err != nil {
		return "FAIL", 0
	}
	if currentStock <= 0 {
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

	// 4. 중복 확인, 재고 확인/차감, 구매자 등록을 하나의 Redis
	// Lua script로 처리하여 같은 사용자의 동시 요청도 한 번만 성공시킵니다.
	reserveStatus, remaining, err := s.LockRepo.ReservePurchase(ctx, ticketName, userID)
	if err != nil {
		return "FAIL", 0
	}
	switch reserveStatus {
	case repository.PurchaseAlreadyPurchased:
		return "ALREADY_PURCHASED", remaining
	case repository.PurchaseSoldOut:
		return "SOLD_OUT", 0
	case repository.PurchaseReserved:
		// continue
	default:
		return "FAIL", 0
	}

	// 6. Kafka로 예매 이벤트 발행 (비동기 저장 시작)
	if err := s.KafkaRepo.PublishPurchase(userID, ticketName); err != nil {
		if _, rollbackErr := s.LockRepo.RollbackPurchase(ctx, ticketName, userID); rollbackErr != nil {
			return "FAIL", 0
		}
		return "FAIL", 0
	}

	metrics.TicketStockLevel.Set(float64(remaining))

	return "SUCCESS", remaining
}

// CancelTicket: 예매 취소 로직
func (s *TicketService) CancelTicket(ctx context.Context, userID string) (bool, string) {
	ticketName := "concert_2026"

	status, err := s.LockRepo.BeginCancel(ctx, ticketName, userID)
	if err != nil {
		return false, "취소 상태를 확인하는 중 오류가 발생했습니다."
	}
	switch status {
	case repository.CancelNotPurchased:
		return false, "구매 내역이 없거나 이미 취소되었습니다."
	case repository.CancelAlreadyPending:
		return false, "이미 취소 처리 중입니다."
	case repository.CancelPending:
		// continue
	default:
		return false, "알 수 없는 취소 상태입니다."
	}

	// Redis는 아직 구매 상태와 재고를 유지합니다. Kafka 발행에 실패하면
	// pending 표식만 제거하므로 부분 취소가 외부에 노출되지 않습니다.
	if err := s.KafkaRepo.PublishCancel(userID, ticketName); err != nil {
		if abortErr := s.LockRepo.AbortCancel(ctx, ticketName, userID); abortErr != nil {
			return false, "취소 이벤트 발행과 상태 복구에 실패했습니다."
		}
		return false, "취소 이벤트 발행에 실패했습니다."
	}

	return true, "취소 요청이 접수되었습니다."
}
