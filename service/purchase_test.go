package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/segmentio/kafka-go"
	"ticket-system/repository"
)

type memoryLockRepository struct {
	mu        sync.Mutex
	stock     int
	purchased map[string]bool
	pending   map[string]bool
}

func newMemoryLockRepository(stock int) *memoryLockRepository {
	return &memoryLockRepository{stock: stock, purchased: map[string]bool{}, pending: map[string]bool{}}
}

func (r *memoryLockRepository) GetStock(context.Context, string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stock, nil
}

func (r *memoryLockRepository) ReservePurchase(_ context.Context, _, userID string) (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.purchased[userID] {
		return repository.PurchaseAlreadyPurchased, r.stock, nil
	}
	if r.stock <= 0 {
		return repository.PurchaseSoldOut, r.stock, nil
	}
	r.stock--
	r.purchased[userID] = true
	return repository.PurchaseReserved, r.stock, nil
}

func (r *memoryLockRepository) RollbackPurchase(_ context.Context, _, userID string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.purchased[userID] {
		delete(r.purchased, userID)
		delete(r.pending, userID)
		r.stock++
	}
	return r.stock, nil
}

func (r *memoryLockRepository) BeginCancel(_ context.Context, _, userID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.purchased[userID] {
		return repository.CancelNotPurchased, nil
	}
	if r.pending[userID] {
		return repository.CancelAlreadyPending, nil
	}
	r.pending[userID] = true
	return repository.CancelPending, nil
}

func (r *memoryLockRepository) AbortCancel(_ context.Context, _, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, userID)
	return nil
}

func (r *memoryLockRepository) FinalizeCancel(_ context.Context, _, userID string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[userID] {
		delete(r.pending, userID)
		if r.purchased[userID] {
			delete(r.purchased, userID)
			r.stock++
		}
	}
	return r.stock, nil
}

func (r *memoryLockRepository) TryEnterOrEnqueue(context.Context, string, int) (string, int, error) {
	return "ACTIVE", 0, nil
}
func (r *memoryLockRepository) RemoveActiveUser(context.Context, string) error { return nil }
func (r *memoryLockRepository) PromoteUsers(context.Context, int) (int, error) { return 0, nil }

type stubTicketRepository struct{}

func (stubTicketRepository) GetStock(string) (int, error)                { return 0, nil }
func (stubTicketRepository) DecreaseStock(string) error                  { return nil }
func (stubTicketRepository) SavePurchase(string, string) (bool, error)   { return true, nil }
func (stubTicketRepository) ExistsPurchase(string, string) (bool, error) { return false, nil }
func (stubTicketRepository) DeletePurchase(string, string) error         { return nil }

type stubPublisher struct {
	purchaseErr error
	cancelErr   error
}

func (p *stubPublisher) PublishPurchase(string, string) error { return p.purchaseErr }
func (p *stubPublisher) PublishCancel(string, string) error   { return p.cancelErr }
func (p *stubPublisher) PublishToDLQ(context.Context, kafka.Message, string) error {
	return nil
}

func newTestService(lock *memoryLockRepository, publisher *stubPublisher) *TicketService {
	return NewTicketService(lock, stubTicketRepository{}, publisher)
}

func TestBuyTicketNormalAndDuplicate(t *testing.T) {
	lock := newMemoryLockRepository(2)
	service := newTestService(lock, &stubPublisher{})

	status, remaining := service.BuyTicket(context.Background(), "user-1")
	if status != "SUCCESS" || remaining != 1 {
		t.Fatalf("first purchase = (%s, %d), want (SUCCESS, 1)", status, remaining)
	}
	status, _ = service.BuyTicket(context.Background(), "user-1")
	if status != "ALREADY_PURCHASED" {
		t.Fatalf("duplicate status = %s, want ALREADY_PURCHASED", status)
	}
	if lock.stock != 1 {
		t.Fatalf("stock = %d, want 1", lock.stock)
	}
}

func TestConcurrentDuplicatePurchaseSucceedsOnce(t *testing.T) {
	lock := newMemoryLockRepository(10)
	service := newTestService(lock, &stubPublisher{})
	var success int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := service.BuyTicket(context.Background(), "same-user")
			if status == "SUCCESS" {
				atomic.AddInt64(&success, 1)
			}
		}()
	}
	wg.Wait()
	if success != 1 || lock.stock != 9 {
		t.Fatalf("success=%d stock=%d, want 1 and 9", success, lock.stock)
	}
}

func TestSoldOutAndConcurrentStockNeverNegative(t *testing.T) {
	lock := newMemoryLockRepository(10)
	service := newTestService(lock, &stubPublisher{})
	var success int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			status, _ := service.BuyTicket(context.Background(), string(rune(user+1000)))
			if status == "SUCCESS" {
				atomic.AddInt64(&success, 1)
			}
		}(i)
	}
	wg.Wait()
	if success != 10 || lock.stock != 0 {
		t.Fatalf("success=%d stock=%d, want 10 and 0", success, lock.stock)
	}
	status, _ := service.BuyTicket(context.Background(), "late-user")
	if status != "SOLD_OUT" || lock.stock < 0 {
		t.Fatalf("sold-out status=%s stock=%d", status, lock.stock)
	}
}

func TestPurchasePublishFailureRollsBack(t *testing.T) {
	lock := newMemoryLockRepository(1)
	service := newTestService(lock, &stubPublisher{purchaseErr: errors.New("kafka unavailable")})
	status, _ := service.BuyTicket(context.Background(), "user-1")
	if status != "FAIL" || lock.stock != 1 || lock.purchased["user-1"] {
		t.Fatalf("status=%s stock=%d purchased=%v", status, lock.stock, lock.purchased["user-1"])
	}
}

func TestCancelLifecycleAndDuplicate(t *testing.T) {
	lock := newMemoryLockRepository(0)
	lock.purchased["user-1"] = true
	service := newTestService(lock, &stubPublisher{})

	ok, _ := service.CancelTicket(context.Background(), "user-1")
	if !ok || !lock.pending["user-1"] || lock.stock != 0 {
		t.Fatalf("cancel accepted=%v pending=%v stock=%d", ok, lock.pending["user-1"], lock.stock)
	}
	ok, _ = service.CancelTicket(context.Background(), "user-1")
	if ok {
		t.Fatal("duplicate pending cancellation unexpectedly succeeded")
	}
	stock, err := lock.FinalizeCancel(context.Background(), "concert_2026", "user-1")
	if err != nil || stock != 1 || lock.purchased["user-1"] {
		t.Fatalf("finalize stock=%d purchased=%v err=%v", stock, lock.purchased["user-1"], err)
	}
}

func TestCancelNotPurchasedAndPublishFailure(t *testing.T) {
	lock := newMemoryLockRepository(1)
	service := newTestService(lock, &stubPublisher{})
	if ok, _ := service.CancelTicket(context.Background(), "missing"); ok {
		t.Fatal("cancel without purchase unexpectedly succeeded")
	}

	lock.purchased["user-1"] = true
	service = newTestService(lock, &stubPublisher{cancelErr: errors.New("kafka unavailable")})
	if ok, _ := service.CancelTicket(context.Background(), "user-1"); ok {
		t.Fatal("cancel publish failure unexpectedly succeeded")
	}
	if lock.pending["user-1"] || !lock.purchased["user-1"] || lock.stock != 1 {
		t.Fatalf("failed cancel changed state: pending=%v purchased=%v stock=%d", lock.pending["user-1"], lock.purchased["user-1"], lock.stock)
	}
}
