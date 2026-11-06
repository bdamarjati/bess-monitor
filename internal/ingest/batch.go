package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// maxBuffered caps memory if the database is unavailable for a long time.
// At 1 msg / 5 s per device this is many hours of data.
const maxBuffered = 50_000

// FlushFunc persists a batch of readings.
type FlushFunc func(context.Context, []Reading) error

// Batcher buffers readings in memory and flushes them periodically.
type Batcher struct {
	mu    sync.Mutex
	buf   []Reading
	flush FlushFunc
	every time.Duration
}

func NewBatcher(every time.Duration, flush FlushFunc) *Batcher {
	return &Batcher{flush: flush, every: every}
}

// Add queues a reading. Safe for concurrent use.
func (b *Batcher) Add(r Reading) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) >= maxBuffered {
		b.buf = b.buf[1:] // drop oldest
	}
	b.buf = append(b.buf, r)
}

// Run flushes every interval until ctx is cancelled, then flushes once more.
func (b *Batcher) Run(ctx context.Context) {
	t := time.NewTicker(b.every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.Flush(ctx)
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			b.Flush(final)
			cancel()
			return
		}
	}
}

// Flush writes buffered readings. On failure they are kept for the next attempt.
func (b *Batcher) Flush(ctx context.Context) {
	b.mu.Lock()
	batch := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(batch) == 0 {
		return
	}

	start := time.Now()
	if err := b.flush(ctx, batch); err != nil {
		slog.Error("flush failed, will retry", "n", len(batch), "err", err)
		b.mu.Lock()
		b.buf = append(batch, b.buf...)
		if over := len(b.buf) - maxBuffered; over > 0 {
			b.buf = b.buf[over:]
		}
		b.mu.Unlock()
		return
	}
	slog.Info("flushed", "n", len(batch), "took", time.Since(start).Round(time.Millisecond))
}
