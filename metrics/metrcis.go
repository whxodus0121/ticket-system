package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// 1. 초당 예매 요청 횟수 (Counter: 계속 증가하는 값)
	PurchaseRequests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ticket_purchase_requests_total",
		Help: "Total number of ticket purchase requests",
	})

	// 2. 현재 Redis에 남아있는 재고 (Gauge: 올라갔다 내려갔다 하는 값)
	TicketStockLevel = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ticket_stock_level",
		Help: "Current ticket stock level in Redis",
	})
)
