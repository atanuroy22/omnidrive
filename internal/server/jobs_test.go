package server

import (
	"sync"
	"testing"
	"time"
)

// A leaking client must not be able to grow the server without bound. The UI
// once opened a fresh event stream on every reconnect without closing the
// previous one, which piled up buffers and goroutines here.
func TestSubscribersAreBounded(t *testing.T) {
	jr := newJobRegistry()

	var channels []chan []byte
	for i := 0; i < maxSubscribers*3; i++ {
		channels = append(channels, jr.subscribe())
	}

	jr.mu.RLock()
	n := len(jr.subs)
	jr.mu.RUnlock()
	if n > maxSubscribers {
		t.Fatalf("%d subscribers registered, cap is %d", n, maxSubscribers)
	}

	// Every handler still unsubscribes on its way out. Evicted channels are
	// unregistered but not closed, so this must not panic on a double close.
	for _, ch := range channels {
		jr.unsubscribe(ch)
	}

	jr.mu.RLock()
	defer jr.mu.RUnlock()
	if len(jr.subs) != 0 {
		t.Fatalf("%d subscribers left after everyone unsubscribed", len(jr.subs))
	}
}

// Publishing must never block on a subscriber that has stopped reading — a
// backgrounded tab must not be able to stall a transfer.
func TestPublishDoesNotBlockOnSlowSubscriber(t *testing.T) {
	jr := newJobRegistry()
	ch := jr.subscribe() // never read from
	defer jr.unsubscribe(ch)

	job := jr.start("upload", "big.bin", "Drive", 1000)

	done := make(chan struct{})
	go func() {
		// Far more than the channel buffer holds.
		for i := 0; i < 500; i++ {
			jr.progress(job, int64(i)*1000)
		}
		jr.finish(job, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-timeoutAfterSeconds(5):
		t.Fatal("publishing blocked on a subscriber that stopped reading")
	}
}

func TestConcurrentSubscribeIsSafe(t *testing.T) {
	jr := newJobRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := jr.subscribe()
			jr.unsubscribe(ch)
		}()
	}
	wg.Wait()
}

func timeoutAfterSeconds(n int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(n) * time.Second)
		close(ch)
	}()
	return ch
}
