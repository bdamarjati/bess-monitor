package ingest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBatcherRetriesAfterFailure(t *testing.T) {
	var got []Reading
	fail := true
	b := NewBatcher(time.Hour, func(_ context.Context, rs []Reading) error {
		if fail {
			return errors.New("db down")
		}
		got = append(got, rs...)
		return nil
	})

	b.Add(Reading{DeviceID: "a"})
	b.Flush(context.Background()) // fails, keeps the reading
	b.Add(Reading{DeviceID: "b"})
	fail = false
	b.Flush(context.Background())

	if len(got) != 2 || got[0].DeviceID != "a" || got[1].DeviceID != "b" {
		t.Fatalf("got %+v, want a then b", got)
	}
}

func TestBatcherFinalFlushOnStop(t *testing.T) {
	var n int
	b := NewBatcher(time.Hour, func(_ context.Context, rs []Reading) error {
		n += len(rs)
		return nil
	})
	b.Add(Reading{DeviceID: "a"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	cancel()
	<-done

	if n != 1 {
		t.Fatalf("flushed %d readings on stop, want 1", n)
	}
}
