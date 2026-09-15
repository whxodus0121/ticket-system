package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisRepository struct {
	Client *redis.Client
}

// Lock: SetNX를 이용해 열쇠를 획득 시도
func (r *RedisRepository) Lock(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	return r.Client.SetNX(ctx, key, "locked", expiration).Result()
}

// Unlock: 열쇠 반납
func (r *RedisRepository) Unlock(ctx context.Context, key string) error {
	return r.Client.Del(ctx, key).Err()
}

func (r *RedisRepository) DecreaseStock(ctx context.Context, ticketName string) (int, error) {
	key := "ticket_stock:" + ticketName

	// Lua 스크립트 작성
	// 1. 현재 재고(GET)를 가져와서 숫자로 변환합니다.
	// 2. 재고가 존재하고(stock) 0보다 크면(stock > 0) 1을 뺍니다(DECR).
	// 3. 재고가 없으면 깎지 않고 -1을 반환합니다.
	script := `
		local stock = redis.call("GET", KEYS[1])
		if stock and tonumber(stock) > 0 then
			return redis.call("DECR", KEYS[1])
		else
			return -1
		end
	`

	// Eval
	val, err := r.Client.Eval(ctx, script, []string{key}).Int()
	if err != nil {
		return -1, err
	}

	// 재고 부족 상황 처리
	if val == -1 {
		return -1, nil // 서비스 계층에서 "매진"으로 판단할 수 있게 -1 반환
	}

	return val, nil
}

const (
	PurchaseReserved         = "SUCCESS"
	PurchaseAlreadyPurchased = "ALREADY_PURCHASED"
	PurchaseSoldOut          = "SOLD_OUT"
	CancelPending            = "PENDING"
	CancelAlreadyPending     = "ALREADY_PENDING"
	CancelNotPurchased       = "NOT_PURCHASED"
)

var reservePurchaseScript = redis.NewScript(`
    local stock_key = KEYS[1]
    local purchased_key = KEYS[2]
    local user_id = ARGV[1]

    if redis.call("SISMEMBER", purchased_key, user_id) == 1 then
        local stock = tonumber(redis.call("GET", stock_key) or "0")
        return {"ALREADY_PURCHASED", stock}
    end

    local stock = tonumber(redis.call("GET", stock_key) or "0")
    if stock <= 0 then
        return {"SOLD_OUT", stock}
    end

    local remaining = redis.call("DECR", stock_key)
    redis.call("SADD", purchased_key, user_id)
    return {"SUCCESS", remaining}
`)

// ReservePurchase atomically checks duplicate purchase state, checks stock,
// decrements stock, and records the purchaser.
func (r *RedisRepository) ReservePurchase(ctx context.Context, ticketName, userID string) (string, int, error) {
	result, err := reservePurchaseScript.Run(ctx, r.Client,
		[]string{"ticket_stock:" + ticketName, "purchased_users:" + ticketName}, userID).Result()
	if err != nil {
		return "", 0, err
	}
	return parseStatusAndNumber(result)
}

var rollbackPurchaseScript = redis.NewScript(`
    local stock_key = KEYS[1]
    local purchased_key = KEYS[2]
    local pending_cancel_key = KEYS[3]
    local user_id = ARGV[1]

    redis.call("SREM", pending_cancel_key, user_id)
    if redis.call("SREM", purchased_key, user_id) == 1 then
        return redis.call("INCR", stock_key)
    end
    return tonumber(redis.call("GET", stock_key) or "0")
`)

// RollbackPurchase compensates a failed Kafka publish without incrementing the
// stock twice when the rollback is retried.
func (r *RedisRepository) RollbackPurchase(ctx context.Context, ticketName, userID string) (int, error) {
	return rollbackPurchaseScript.Run(ctx, r.Client, []string{
		"ticket_stock:" + ticketName,
		"purchased_users:" + ticketName,
		"pending_cancels:" + ticketName,
	}, userID).Int()
}

var beginCancelScript = redis.NewScript(`
    local purchased_key = KEYS[1]
    local pending_cancel_key = KEYS[2]
    local user_id = ARGV[1]

    if redis.call("SISMEMBER", purchased_key, user_id) == 0 then
        return "NOT_PURCHASED"
    end
    if redis.call("SISMEMBER", pending_cancel_key, user_id) == 1 then
        return "ALREADY_PENDING"
    end
    redis.call("SADD", pending_cancel_key, user_id)
    return "PENDING"
`)

// BeginCancel marks a cancellation as pending without releasing stock. Stock is
// released only after the ordered cancel event has been persisted by the worker.
func (r *RedisRepository) BeginCancel(ctx context.Context, ticketName, userID string) (string, error) {
	return beginCancelScript.Run(ctx, r.Client, []string{
		"purchased_users:" + ticketName,
		"pending_cancels:" + ticketName,
	}, userID).Text()
}

func (r *RedisRepository) AbortCancel(ctx context.Context, ticketName, userID string) error {
	return r.Client.SRem(ctx, "pending_cancels:"+ticketName, userID).Err()
}

var finalizeCancelScript = redis.NewScript(`
    local stock_key = KEYS[1]
    local purchased_key = KEYS[2]
    local pending_cancel_key = KEYS[3]
    local user_id = ARGV[1]

    if redis.call("SREM", pending_cancel_key, user_id) == 0 then
        return tonumber(redis.call("GET", stock_key) or "0")
    end
    if redis.call("SREM", purchased_key, user_id) == 1 then
        return redis.call("INCR", stock_key)
    end
    return tonumber(redis.call("GET", stock_key) or "0")
`)

// FinalizeCancel is idempotent. A redelivered cancel cannot increment stock
// twice because only a pending cancellation may release a purchased seat.
func (r *RedisRepository) FinalizeCancel(ctx context.Context, ticketName, userID string) (int, error) {
	return finalizeCancelScript.Run(ctx, r.Client, []string{
		"ticket_stock:" + ticketName,
		"purchased_users:" + ticketName,
		"pending_cancels:" + ticketName,
	}, userID).Int()
}

func parseStatusAndNumber(result interface{}) (string, int, error) {
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return "", 0, fmt.Errorf("unexpected lua script result: %T", result)
	}
	status, ok := values[0].(string)
	if !ok {
		return "", 0, fmt.Errorf("unexpected lua status type: %T", values[0])
	}
	var number int
	switch value := values[1].(type) {
	case int64:
		number = int(value)
	case int:
		number = value
	default:
		return "", 0, fmt.Errorf("unexpected lua number type: %T", values[1])
	}
	return status, number, nil
}

func (r *RedisRepository) AddPurchasedUser(ctx context.Context, ticketName string, userID string) error {
	// 구매자 명단 Key 예시: "purchased_users:concert_2026"
	key := "purchased_users:" + ticketName

	// Redis Set에 userID 추가
	err := r.Client.SAdd(ctx, key, userID).Err()
	return err
}

func (r *RedisRepository) IsUserPurchased(ctx context.Context, ticketName string, userID string) (bool, error) {
	key := "purchased_users:" + ticketName
	// Set에 해당 유저가 있는지 확인 (SIsMember)
	exists, err := r.Client.SIsMember(ctx, key, userID).Result()
	return exists, err
}

func (r *RedisRepository) IncreaseStock(ctx context.Context, ticketName string) (int, error) {
	key := "ticket_stock:" + ticketName
	val, err := r.Client.Incr(ctx, key).Result() // 재고 1 증가
	return int(val), err
}

func (r *RedisRepository) RemovePurchasedUser(ctx context.Context, ticketName string, userID string) error {
	key := "purchased_users:" + ticketName
	return r.Client.SRem(ctx, key, userID).Err() // 구매 명단에서 유저 삭제
}

var enqueueScript = redis.NewScript(`
    local active_set_key = KEYS[1]
    local waiting_queue_key = KEYS[2]
    local user_id = ARGV[1]
    local max_active = tonumber(ARGV[2])
    local timestamp = ARGV[3]

    -- 1. 이미 Active Set에 있는지 확인
    if redis.call("SISMEMBER", active_set_key, user_id) == 1 then
        return {"ACTIVE", 0}
    end

    -- 2. Active Set 자리가 있는지 확인
    local current_active_size = redis.call("SCARD", active_set_key)
    if current_active_size < max_active then
        redis.call("SADD", active_set_key, user_id)
        return {"ACTIVE", 0}
    end

    -- 3. 자리가 없으면 대기열 진입
    redis.call("ZADD", waiting_queue_key, timestamp, user_id)
    local rank = redis.call("ZRANK", waiting_queue_key, user_id)
    
    -- 리스트 형태로 반환 (상태, 순번)
    return {"WAITING", rank + 1}
`)

func (r *RedisRepository) TryEnterOrEnqueue(ctx context.Context, userID string, maxActive int) (string, int, error) {
	keys := []string{"ticket:active_set", "ticket:waiting_queue"}
	args := []interface{}{
		userID,
		maxActive,
		time.Now().UnixNano(),
	}

	result, err := enqueueScript.Run(ctx, r.Client, keys, args...).Result()
	if err != nil {
		return "", 0, err
	}

	// Lua에서 반환된 인터페이스 슬라이스 처리
	res, ok := result.([]interface{})
	if !ok || len(res) < 2 {
		return "ERROR", 0, fmt.Errorf("unexpected lua script result format")
	}

	status := res[0].(string)

	var rank int
	switch v := res[1].(type) {
	case int64:
		rank = int(v)
	case int:
		rank = v
	default:
		rank = 0
	}

	return status, rank, nil
}

// RemoveActiveUser: 예매 완료 또는 취소 시 Active Set에서 유저 제거
func (r *RedisRepository) RemoveActiveUser(ctx context.Context, userID string) error {
	return r.Client.SRem(ctx, "ticket:active_set", userID).Err()
}

var promoteScript = redis.NewScript(`
    local waiting_queue_key = KEYS[1]
    local active_set_key = KEYS[2]
    local max_active = tonumber(ARGV[1])

    -- 1. 현재 Active Set의 빈자리 계산
    local current_active_size = redis.call("SCARD", active_set_key)
    local seats_available = max_active - current_active_size

    if seats_available <= 0 then
        return 0
    end

    -- 2. 빈자리만큼 대기열에서 가장 오래된 유저들을 뽑아옴
    local users = redis.call("ZPOPMIN", waiting_queue_key, seats_available)
    local promoted_count = 0

    -- ZPOPMIN은 {user1, score1, user2, score2...} 형태로 반환하므로 인덱스 2개씩 점프
    for i = 1, #users, 2 do
        redis.call("SADD", active_set_key, users[i])
        promoted_count = promoted_count + 1
    end

    return promoted_count
`)

func (r *RedisRepository) PromoteUsers(ctx context.Context, maxActive int) (int, error) {
	keys := []string{"ticket:waiting_queue", "ticket:active_set"}
	args := []interface{}{maxActive}

	result, err := promoteScript.Run(ctx, r.Client, keys, args...).Int()
	if err != nil {
		return 0, err
	}
	return result, nil
}

func (r *RedisRepository) GetStock(ctx context.Context, ticketName string) (int, error) {
	key := "ticket_stock:" + ticketName

	// Redis에서 해당 키의 값을 가져옵니다.
	val, err := r.Client.Get(ctx, key).Int()
	if err == redis.Nil {
		// 만약 Redis에 키가 없다면, 재고가 설정되지 않은 것이므로 0(또는 에러)을 반환
		return 0, nil
	} else if err != nil {
		return 0, err
	}

	return val, nil
}
