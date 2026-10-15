package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/anomaly"
)

func ev(kind string) anomaly.Event {
	return anomaly.Event{Kind: kind, Value: 99, Threshold: 85, Timestamp: time.Now()}
}

func TestBatchesBySize(t *testing.T) {
	var mu sync.Mutex
	var got []Batch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var delivered atomic.Int64
	c := New(Config{URL: srv.URL, Node: "n1", BatchSize: 2, FlushInterval: time.Hour},
		Hooks{OnDelivered: func(n int) { delivered.Add(int64(n)) }})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)

	c.Enqueue(ev("cpu_spike"))
	c.Enqueue(ev("io_wait"))
	c.Enqueue(ev("memory_pressure")) // flushed on shutdown
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-c.Done()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || len(got[0].Events) != 2 || len(got[1].Events) != 1 || got[0].Node != "n1" {
		t.Fatalf("unexpected batches: %+v", got)
	}
	if delivered.Load() != 3 {
		t.Errorf("delivered = %d", delivered.Load())
	}
}

func TestRetryThenDrop(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var dropped atomic.Int64
	c := New(Config{URL: srv.URL, Node: "n", BatchSize: 1, MaxRetries: 3, Backoff: time.Millisecond},
		Hooks{OnDropped: func(n int) { dropped.Add(int64(n)) }})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	c.Enqueue(ev("cpu_spike"))
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-c.Done()

	if calls.Load() != 3 || dropped.Load() != 1 {
		t.Errorf("calls=%d dropped=%d", calls.Load(), dropped.Load())
	}
}

func TestRetrySucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	var delivered atomic.Int64
	c := New(Config{URL: srv.URL, BatchSize: 1, Backoff: time.Millisecond},
		Hooks{OnDelivered: func(n int) { delivered.Add(int64(n)) }})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	c.Enqueue(ev("cpu_spike"))
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-c.Done()
	if delivered.Load() != 1 || calls.Load() != 2 {
		t.Errorf("delivered=%d calls=%d", delivered.Load(), calls.Load())
	}
}

func TestEnqueueDropsWhenFull(t *testing.T) {
	var dropped atomic.Int64
	c := New(Config{URL: "http://x", QueueSize: 1}, Hooks{OnDropped: func(n int) { dropped.Add(int64(n)) }})
	if !c.Enqueue(ev("a")) || c.Enqueue(ev("b")) || dropped.Load() != 1 {
		t.Errorf("expected second enqueue to drop, dropped=%d", dropped.Load())
	}
}
