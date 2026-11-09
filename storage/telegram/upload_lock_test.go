package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	storconfig "github.com/krau/SaveAny-Bot/config/storage"
)

func TestWithChatUploadLockReentrant(t *testing.T) {
	tg := &Telegram{config: storconfig.TelegramStorageConfig{ChatID: 111}}
	done := make(chan struct{})
	go func() {
		err := tg.withChatUploadLock(context.Background(), func(ctx context.Context) error {
			return tg.withChatUploadLock(ctx, func(context.Context) error {
				close(done)
				return nil
			})
		})
		if err != nil {
			t.Errorf("lock failed: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant chat upload lock deadlocked")
	}
}

func TestWithChatUploadLockSerializesDifferentTasks(t *testing.T) {
	tg := &Telegram{config: storconfig.TelegramStorageConfig{ChatID: 222}}
	var order []int
	var mu sync.Mutex
	record := func(n int) {
		mu.Lock()
		order = append(order, n)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = tg.withChatUploadLock(context.Background(), func(context.Context) error {
			record(1)
			time.Sleep(80 * time.Millisecond)
			record(2)
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		_ = tg.withChatUploadLock(context.Background(), func(context.Context) error {
			record(3)
			record(4)
			return nil
		})
	}()
	wg.Wait()

	mu.Lock()
	got := append([]int(nil), order...)
	mu.Unlock()
	want := []int{1, 2, 3, 4}
	if len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 3 || got[3] != 4 {
		t.Fatalf("order=%v, want contiguous %v (no interleave)", got, want)
	}
}

func TestWithChatUploadLockIndependentChats(t *testing.T) {
	a := &Telegram{config: storconfig.TelegramStorageConfig{ChatID: 333}}
	b := &Telegram{config: storconfig.TelegramStorageConfig{ChatID: 444}}
	started := make(chan struct{}, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = a.withChatUploadLock(context.Background(), func(context.Context) error {
			started <- struct{}{}
			time.Sleep(50 * time.Millisecond)
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_ = b.withChatUploadLock(context.Background(), func(context.Context) error {
			started <- struct{}{}
			time.Sleep(50 * time.Millisecond)
			return nil
		})
	}()
	deadline := time.After(200 * time.Millisecond)
	for range 2 {
		select {
		case <-started:
		case <-deadline:
			t.Fatal("different chats should not block each other")
		}
	}
	wg.Wait()
}
