package handler

import (
	"encoding/json"
	"net/http"
	"ticket-system/service"
)

// Response: 공통 응답 구조체 (성공 및 매진 시 표준 응답 형식)
type Response struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Stock   int    `json:"stock"`
}

/*
 * TicketHandler: 티켓 예매 요청을 처리하는 컨트롤러 레이어
 * 클라이언트의 HTTP 요청을 해석하고, 비즈니스 로직(Service)의 결과를
 * 적절한 HTTP 상태 코드 및 JSON 메시지로 변환하여 반환합니다.
 */
type TicketHandler struct {
	Service *service.TicketService
}

func NewTicketHandler(s *service.TicketService) *TicketHandler {
	return &TicketHandler{
		Service: s,
	}
}

// ServeHTTP: 티켓 예매 요청(GET /ticket?user_id=...) 처리 핸들러
func (h *TicketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 1. 유저 식별 (실제 서비스에서는 JWT나 세션 등을 활용하도록 확장 가능)
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "anonymous"
	}

	// 2. 비즈니스 로직 호출 (대기열 기반 예매 처리)
	// status: SUCCESS(성공), WAITING(대기), SOLD_OUT(매진), ALREADY_PURCHASED(중복)
	// remaining: 남은 재고 수량 또는 대기열에서의 순번(rank)
	status, remaining := h.Service.BuyTicket(userID)

	// 3. 서비스 결과에 따른 HTTP 상태 코드 및 페이로드 구성
	switch status {
	case "SUCCESS":
		json.NewEncoder(w).Encode(Response{
			Success: true,
			Message: "예매 성공!",
			Stock:   remaining,
		})
	case "WAITING":
		// [202 Accepted] 요청이 수락되었으나 대기 중임을 명시 (순번 정보 포함)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"status":  "WAITING",
			"message": "현재 대기열에 진입했습니다.",
			"rank":    remaining,
		})

	case "ALREADY_PURCHASED":
		// [400 Bad Request] 중복 구매 시도 제한
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "티켓은 1인 1매입니다."})

	case "SOLD_OUT":
		// [410 Gone] 자원이 더 이상 존재하지 않음을 명시 (매진)
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(Response{
			Success: false,
			Message: "매진되었습니다.",
			Stock:   0,
		})

	default:
		// [500 Internal Server Error] 시스템 예외 상황
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "시스템 오류가 발생했습니다."})
	}
}
