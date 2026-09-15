package repository

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
)

func integrationRedis(t *testing.T) (*RedisRepository, func()) {
	t.Helper()
	address := os.Getenv("REDIS_INTEGRATION_ADDR")
	if address == "" {
		t.Skip("set REDIS_INTEGRATION_ADDR to run Redis Lua integration tests")
	}
	client := redis.NewClient(&redis.Options{Addr: address})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis: %v", err)
	}
	repository := &RedisRepository{Client: client}
	cleanup := func() {
		client.Del(ctx,
			"ticket_stock:integration_ticket",
			"purchased_users:integration_ticket",
			"pending_cancels:integration_ticket",
		)
		client.Close()
	}
	cleanup()
	client = redis.NewClient(&redis.Options{Addr: address})
	repository.Client = client
	return repository, cleanup
}

func TestRedisAtomicDuplicatePurchase(t *testing.T) {
	repository, cleanup := integrationRedis(t)
	defer cleanup()
	ctx := context.Background()
	if err := repository.Client.Set(ctx, "ticket_stock:integration_ticket", 10, 0).Err(); err != nil {
		t.Fatal(err)
	}

	var success int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, err := repository.ReservePurchase(ctx, "integration_ticket", "same-user")
			if err != nil {
				t.Errorf("reserve purchase: %v", err)
				return
			}
			if status == PurchaseReserved {
				atomic.AddInt64(&success, 1)
			}
		}()
	}
	wg.Wait()
	stock, _ := repository.GetStock(ctx, "integration_ticket")
	purchasers, _ := repository.Client.SCard(ctx, "purchased_users:integration_ticket").Result()
	if success != 1 || stock != 9 || purchasers != 1 {
		t.Fatalf("success=%d stock=%d purchasers=%d", success, stock, purchasers)
	}
}

func TestRedisAtomicSoldOutNeverNegative(t *testing.T) {
	repository, cleanup := integrationRedis(t)
	defer cleanup()
	ctx := context.Background()
	if err := repository.Client.Set(ctx, "ticket_stock:integration_ticket", 10, 0).Err(); err != nil {
		t.Fatal(err)
	}

	var success int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			status, _, err := repository.ReservePurchase(ctx, "integration_ticket", fmt.Sprintf("user-%d", user))
			if err != nil {
				t.Errorf("reserve purchase: %v", err)
				return
			}
			if status == PurchaseReserved {
				atomic.AddInt64(&success, 1)
			}
		}(i)
	}
	wg.Wait()
	stock, _ := repository.GetStock(ctx, "integration_ticket")
	if success != 10 || stock != 0 {
		t.Fatalf("success=%d stock=%d, want 10 and 0", success, stock)
	}
}

func TestRedisAtomicCancelLifecycle(t *testing.T) {
	repository, cleanup := integrationRedis(t)
	defer cleanup()
	ctx := context.Background()
	repository.Client.Set(ctx, "ticket_stock:integration_ticket", 0, 0)
	repository.Client.SAdd(ctx, "purchased_users:integration_ticket", "user-1")

	status, err := repository.BeginCancel(ctx, "integration_ticket", "user-1")
	if err != nil || status != CancelPending {
		t.Fatalf("begin status=%s err=%v", status, err)
	}
	status, err = repository.BeginCancel(ctx, "integration_ticket", "user-1")
	if err != nil || status != CancelAlreadyPending {
		t.Fatalf("duplicate begin status=%s err=%v", status, err)
	}
	stock, err := repository.FinalizeCancel(ctx, "integration_ticket", "user-1")
	if err != nil || stock != 1 {
		t.Fatalf("finalize stock=%d err=%v", stock, err)
	}
	stock, err = repository.FinalizeCancel(ctx, "integration_ticket", "user-1")
	if err != nil || stock != 1 {
		t.Fatalf("duplicate finalize stock=%d err=%v", stock, err)
	}
	status, err = repository.BeginCancel(ctx, "integration_ticket", "missing")
	if err != nil || status != CancelNotPurchased {
		t.Fatalf("missing begin status=%s err=%v", status, err)
	}
}
