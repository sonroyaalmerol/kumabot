package player

import (
	"context"
	"testing"
	"time"
)

// benchmarkOpusPacketSize is a realistic opus packet size (variable bitrate, ~20ms frames).
var benchmarkOpusPacket = make([]byte, 120) // typical compressed opus frame

// warmupPool fills the pool so subsequent Push/Pop cycles hit zero allocations.
// Ring buffer usable capacity is maxSize-1, so we use n-1 items.
func warmupPool(ob *opusBuffer, n int) {
	cap := n - 1 // ring buffer reserves 1 slot
	for i := range cap {
		ob.Push(benchmarkOpusPacket, int64(i), time.Now())
	}
	for range cap {
		pkt, _ := ob.Pop(context.Background())
		ob.Release(pkt.data)
	}
}

// BenchmarkOpusBufferPushPop measures the steady-state Push+Pop+Release cycle.
// This is the hot path at 50 ops/sec per guild.
func BenchmarkOpusBufferPushPop(b *testing.B) {
	ob := newOpusBuffer(1024)
	warmupPool(ob, 1024)
	ts := time.Now()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		ob.Push(benchmarkOpusPacket, int64(i), ts)
		pkt, ok := ob.Pop(context.Background())
		if !ok {
			b.Fatal("Pop failed")
		}
		ob.Release(pkt.data)
	}
}

// BenchmarkOpusBufferPushOnly measures Push alone (producer side).
func BenchmarkOpusBufferPushOnly(b *testing.B) {
	ob := newOpusBuffer(1024)
	warmupPool(ob, 1024)
	ts := time.Now()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		ob.Push(benchmarkOpusPacket, int64(i), ts)
	}
}

// BenchmarkOpusBufferRelease measures the Release path (return to pool).
func BenchmarkOpusBufferRelease(b *testing.B) {
	ob := newOpusBuffer(100)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		ob.Release(benchmarkOpusPacket)
	}
}
