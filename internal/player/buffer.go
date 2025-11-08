package player

import (
	"context"
	"sync"
	"time"
)

type opusBuffer struct {
	mu       sync.Mutex
	packets  []bufferedPacket
	maxSize  int
	readPos  int
	writePos int
	closed   bool
	notEmpty *sync.Cond
}

type bufferedPacket struct {
	data     []byte
	pts48    int64
	targetTS time.Time
}

func newOpusBuffer(maxPackets int) *opusBuffer {
	ob := &opusBuffer{
		packets: make([]bufferedPacket, maxPackets),
		maxSize: maxPackets,
	}
	ob.notEmpty = sync.NewCond(&ob.mu)
	return ob
}

func (ob *opusBuffer) Push(data []byte, pts48 int64, targetTS time.Time) bool {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	if ob.closed {
		return false
	}

	// Calculate buffer usage
	used := (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
	if used >= ob.maxSize-1 {
		// Buffer full, drop oldest or wait
		return false
	}

	ob.packets[ob.writePos] = bufferedPacket{
		data:     append([]byte(nil), data...),
		pts48:    pts48,
		targetTS: targetTS,
	}
	ob.writePos = (ob.writePos + 1) % ob.maxSize
	ob.notEmpty.Signal()
	return true
}

func (ob *opusBuffer) Pop(ctx context.Context) (bufferedPacket, bool) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	for {
		if ob.closed {
			return bufferedPacket{}, false
		}

		used := (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
		if used > 0 {
			pkt := ob.packets[ob.readPos]
			ob.readPos = (ob.readPos + 1) % ob.maxSize
			return pkt, true
		}

		// Wait for data
		ob.notEmpty.Wait()
	}
}

func (ob *opusBuffer) IsFull() bool {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	used := (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
	return used >= ob.maxSize-1
}

func (ob *opusBuffer) BufferedCount() int {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	return (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
}

func (ob *opusBuffer) Close() {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.closed = true
	ob.notEmpty.Broadcast()
}
