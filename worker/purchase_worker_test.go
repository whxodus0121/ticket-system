package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"
	"ticket-system/repository"
)

type brokerState struct {
	mu        sync.Mutex
	message   kafka.Message
	committed bool
}

type fakeReader struct {
	state     *brokerState
	commitErr error
}

func (r *fakeReader) FetchMessage(context.Context) (kafka.Message, error) {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if r.state.committed {
		return kafka.Message{}, errors.New("no message")
	}
	return r.state.message, nil
}

func (r *fakeReader) CommitMessages(_ context.Context, _ ...kafka.Message) error {
	if r.commitErr != nil {
		return r.commitErr
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.committed = true
	return nil
}
func (*fakeReader) Close() error { return nil }

type fakeTicketRepository struct {
	saveErr   error
	deleteErr error
	saveCalls int
}

func (*fakeTicketRepository) GetStock(string) (int, error) { return 0, nil }
func (*fakeTicketRepository) DecreaseStock(string) error   { return nil }
func (r *fakeTicketRepository) SavePurchase(string, string) (bool, error) {
	r.saveCalls++
	return r.saveErr == nil, r.saveErr
}
func (*fakeTicketRepository) ExistsPurchase(string, string) (bool, error) { return false, nil }
func (r *fakeTicketRepository) DeletePurchase(string, string) error       { return r.deleteErr }

type fakeLockRepository struct {
	finalizeCalls int
}

func (*fakeLockRepository) GetStock(context.Context, string) (int, error) { return 0, nil }
func (*fakeLockRepository) ReservePurchase(context.Context, string, string) (string, int, error) {
	return repository.PurchaseReserved, 0, nil
}
func (*fakeLockRepository) RollbackPurchase(context.Context, string, string) (int, error) {
	return 0, nil
}
func (*fakeLockRepository) BeginCancel(context.Context, string, string) (string, error) {
	return repository.CancelPending, nil
}
func (*fakeLockRepository) AbortCancel(context.Context, string, string) error { return nil }
func (r *fakeLockRepository) FinalizeCancel(context.Context, string, string) (int, error) {
	r.finalizeCalls++
	return 1, nil
}
func (*fakeLockRepository) TryEnterOrEnqueue(context.Context, string, int) (string, int, error) {
	return "ACTIVE", 0, nil
}
func (*fakeLockRepository) RemoveActiveUser(context.Context, string) error { return nil }
func (*fakeLockRepository) PromoteUsers(context.Context, int) (int, error) { return 0, nil }

type fakePublisher struct {
	dlqErr   error
	dlqCalls int
}

func (*fakePublisher) PublishPurchase(string, string) error { return nil }
func (*fakePublisher) PublishCancel(string, string) error   { return nil }
func (p *fakePublisher) PublishToDLQ(context.Context, kafka.Message, string) error {
	p.dlqCalls++
	return p.dlqErr
}

func testMessage(value string) kafka.Message {
	return kafka.Message{Topic: "ticket-topic", Partition: 1, Offset: 42, Key: []byte("user-1"), Value: []byte(value)}
}

func newTestWorker(state *brokerState, tickets *fakeTicketRepository, publisher *fakePublisher) *PurchaseWorker {
	return &PurchaseWorker{
		Reader:     &fakeReader{state: state},
		TicketRepo: tickets,
		LockRepo:   &fakeLockRepository{},
		KafkaRepo:  publisher,
		RetryCount: 1,
	}
}

func TestDBSuccessCommitsOffset(t *testing.T) {
	state := &brokerState{message: testMessage("concert_2026")}
	worker := newTestWorker(state, &fakeTicketRepository{}, &fakePublisher{})
	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !state.committed {
		t.Fatal("source offset was not committed after DB success")
	}
}

func TestDBFailureAndDLQSuccessCommitsOffset(t *testing.T) {
	state := &brokerState{message: testMessage("concert_2026")}
	publisher := &fakePublisher{}
	worker := newTestWorker(state, &fakeTicketRepository{saveErr: errors.New("db down")}, publisher)
	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !state.committed || publisher.dlqCalls != 1 {
		t.Fatalf("committed=%v dlqCalls=%d, want true and 1", state.committed, publisher.dlqCalls)
	}
}

func TestDBFailureAndDLQFailureDoesNotCommit(t *testing.T) {
	state := &brokerState{message: testMessage("concert_2026")}
	publisher := &fakePublisher{dlqErr: errors.New("dlq down")}
	worker := newTestWorker(state, &fakeTicketRepository{saveErr: errors.New("db down")}, publisher)
	if err := worker.ProcessOne(context.Background()); err == nil {
		t.Fatal("expected processing error")
	}
	if state.committed {
		t.Fatal("source offset was committed despite DB and DLQ failure")
	}
}

func TestUncommittedMessageIsAvailableAfterWorkerRestart(t *testing.T) {
	state := &brokerState{message: testMessage("concert_2026")}
	first := newTestWorker(state, &fakeTicketRepository{saveErr: errors.New("db down")}, &fakePublisher{dlqErr: errors.New("dlq down")})
	if err := first.ProcessOne(context.Background()); err == nil {
		t.Fatal("first worker should fail")
	}

	secondTickets := &fakeTicketRepository{}
	second := newTestWorker(state, secondTickets, &fakePublisher{})
	if err := second.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if secondTickets.saveCalls != 1 || !state.committed {
		t.Fatalf("saveCalls=%d committed=%v", secondTickets.saveCalls, state.committed)
	}
}

func TestCancelSuccessFinalizesRedisThenCommits(t *testing.T) {
	state := &brokerState{message: testMessage("CANCEL:concert_2026")}
	worker := newTestWorker(state, &fakeTicketRepository{}, &fakePublisher{})
	lock := &fakeLockRepository{}
	worker.LockRepo = lock
	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lock.finalizeCalls != 1 || !state.committed {
		t.Fatalf("finalizeCalls=%d committed=%v", lock.finalizeCalls, state.committed)
	}
}

func TestHashBalancerKeepsUserEventsOnSamePartition(t *testing.T) {
	balancer := &kafka.Hash{}
	partitions := []int{0, 1, 2}
	buy := balancer.Balance(kafka.Message{Key: []byte("user-1"), Value: []byte("concert_2026")}, partitions...)
	cancel := balancer.Balance(kafka.Message{Key: []byte("user-1"), Value: []byte("CANCEL:concert_2026")}, partitions...)
	if buy != cancel {
		t.Fatalf("BUY partition=%d CANCEL partition=%d", buy, cancel)
	}
}
