package player

import (
	"context"
	"testing"
	"time"
)

func TestOpusBufferPopRespectsContext(t *testing.T) {
	ob := newOpusBuffer(100)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, ok := ob.Pop(ctx)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("expected Pop to return false on context cancellation")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Pop took too long to respect context: %v", elapsed)
	}
}

func TestOpusBufferPopReturnsAfterEOS(t *testing.T) {
	ob := newOpusBuffer(100)

	// Push one packet
	data := []byte{1, 2, 3}
	if !ob.Push(data, 0, time.Now()) {
		t.Fatal("Push failed")
	}

	// Pop it
	pkt, ok := ob.Pop(context.Background())
	if !ok {
		t.Fatal("expected Pop to succeed")
	}
	if len(pkt.data) != len(data) {
		t.Fatalf("expected %d bytes, got %d", len(data), len(pkt.data))
	}

	// Mark EOS
	go func() {
		time.Sleep(50 * time.Millisecond)
		ob.MarkEOS()
	}()

	start := time.Now()
	_, ok = ob.Pop(context.Background())
	elapsed := time.Since(start)

	if ok {
		t.Fatal("expected Pop to return false after EOS")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Pop took too long after EOS: %v", elapsed)
	}
}

func TestOpusBufferPopReturnsAfterClose(t *testing.T) {
	ob := newOpusBuffer(100)

	go func() {
		time.Sleep(50 * time.Millisecond)
		ob.Close()
	}()

	start := time.Now()
	_, ok := ob.Pop(context.Background())
	elapsed := time.Since(start)

	if ok {
		t.Fatal("expected Pop to return false after Close")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Pop took too long after Close: %v", elapsed)
	}
}
