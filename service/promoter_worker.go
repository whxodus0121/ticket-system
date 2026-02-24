package service

import (
	"context"
	"fmt"
	"time"
)

// StartPromoter는 백그라운드에서 주기적으로 대기열의 유저를 Active Set으로 이동시킵니다.
func (s *TicketService) StartPromoter(ctx context.Context, maxActive int) {
	ticker := time.NewTicker(100 * time.Millisecond) // 0.1초 주기로 실행
	defer ticker.Stop()

	fmt.Println("Promoter 워커가 가동되었습니다. (대상: ticket:active_set)")

	for {
		select {
		case <-ticker.C:
			// Repository에 추가한 PromoteUsers 호출
			count, err := s.LockRepo.PromoteUsers(ctx, maxActive)
			if err != nil {
				fmt.Printf("[Promoter 에러] 유저 승급 중 오류: %v\n", err)
				continue
			}

			if count > 0 {
				fmt.Printf("[Promoter] 대기열에서 %d명을 Active Set으로 승급시켰습니다.\n", count)
			}
		case <-ctx.Done():
			fmt.Println("Promoter 워커를 종료합니다.")
			return
		}
	}
}
