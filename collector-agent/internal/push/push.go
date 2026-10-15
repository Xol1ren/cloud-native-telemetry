// Package push implements an asynchronous batching client that ships anomaly
// events to the Python control plane.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Xol1ren/cloud-native-telemetry/collector-agent/internal/anomaly"
)

// Batch is the wire format accepted by POST /api/v1/telemetry/ingest.
type Batch struct {
	Node   string          `json:"node"`
	Events []anomaly.Event `json:"events"`
}

// Config tunes the client.
type Config struct {
	URL           string
	Node          string
	BatchSize     int
	FlushInterval time.Duration
	QueueSize     int
	MaxRetries    int
	Backoff       time.Duration
	Timeout       time.Duration
}

// Hooks lets callers observe delivery results (e.g. for metrics).
type Hooks struct {
	OnDelivered func(n int)
	OnDropped   func(n int)
}

// Client batches and sends events from a background goroutine.
type Client struct {
	cfg   Config
	hooks Hooks
	http  *http.Client
	queue chan anomaly.Event
	done  chan struct{}
}

// New builds a Client. Call Run in a goroutine and Enqueue from producers.
func New(cfg Config, hooks Hooks) *Client {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Client{
		cfg: cfg, hooks: hooks,
		http:  &http.Client{Timeout: cfg.Timeout},
		queue: make(chan anomaly.Event, cfg.QueueSize),
		done:  make(chan struct{}),
	}
}

// Enqueue adds an event without blocking. It returns false if the queue is full.
func (c *Client) Enqueue(e anomaly.Event) bool {
	select {
	case c.queue <- e:
		return true
	default:
		if c.hooks.OnDropped != nil {
			c.hooks.OnDropped(1)
		}
		return false
	}
}

// Done is closed once Run has flushed and returned.
func (c *Client) Done() <-chan struct{} { return c.done }

// Run processes the queue until ctx is cancelled, then flushes remaining events.
func (c *Client) Run(ctx context.Context) {
	defer close(c.done)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	batch := make([]anomaly.Event, 0, c.cfg.BatchSize)

	flush := func(fctx context.Context) {
		if len(batch) == 0 {
			return
		}
		c.send(fctx, batch)
		batch = batch[:0]
	}
	for {
		select {
		case e := <-c.queue:
			batch = append(batch, e)
			if len(batch) >= c.cfg.BatchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			for { // drain what is already queued
				select {
				case e := <-c.queue:
					batch = append(batch, e)
					continue
				default:
				}
				break
			}
			sctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
			flush(sctx)
			cancel()
			return
		}
	}
}

func (c *Client) send(ctx context.Context, events []anomaly.Event) {
	body, err := json.Marshal(Batch{Node: c.cfg.Node, Events: events})
	if err != nil {
		c.dropped(len(events))
		return
	}
	backoff := c.cfg.Backoff
	for attempt := 1; attempt <= c.cfg.MaxRetries; attempt++ {
		err = c.post(ctx, body)
		if err == nil {
			if c.hooks.OnDelivered != nil {
				c.hooks.OnDelivered(len(events))
			}
			return
		}
		slog.Warn("push failed", "attempt", attempt, "err", err)
		if attempt == c.cfg.MaxRetries {
			break
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-ctx.Done():
			c.dropped(len(events))
			return
		}
	}
	c.dropped(len(events))
}

func (c *Client) dropped(n int) {
	if c.hooks.OnDropped != nil {
		c.hooks.OnDropped(n)
	}
}

func (c *Client) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("control plane returned %s", resp.Status)
	}
	return nil
}
