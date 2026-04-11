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
	eos      bool
	notEmpty *sync.Cond
	pool     sync.Pool
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
		pool: sync.Pool{
			New: func() any { return make([]byte, 0, 200) },
		},
	}
	ob.notEmpty = sync.NewCond(&ob.mu)
	return ob
}

func (ob *opusBuffer) Push(data []byte, pts48 int64, targetTS time.Time) bool {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	if ob.closed || ob.eos {
		return false
	}

	used := (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
	if used >= ob.maxSize-1 {
		return false
	}

	// Recycle the old slot's buffer back to pool
	old := ob.packets[ob.writePos]
	if old.data != nil {
		ob.pool.Put(old.data[:0])
	}

	// Get a buffer from pool and copy data into it
	buf := ob.pool.Get().([]byte)
	buf = append(buf[:0], data...)

	ob.packets[ob.writePos] = bufferedPacket{
		data:     buf,
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
			ob.packets[ob.readPos] = bufferedPacket{}
			ob.readPos = (ob.readPos + 1) % ob.maxSize
			return pkt, true
		}

		if ob.eos {
			return bufferedPacket{}, false
		}

		ob.notEmpty.Wait()
	}
}

// Release returns a packet's data buffer to the pool for reuse.
func (ob *opusBuffer) Release(data []byte) {
	if data != nil {
		ob.pool.Put(data[:0])
	}
}

func (ob *opusBuffer) BufferedCount() int {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	return (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
}

func (ob *opusBuffer) MarkEOS() {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.eos = true
	ob.notEmpty.Broadcast()
}

func (ob *opusBuffer) Close() {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.closed = true
	// Return all buffered data to pool
	for i := 0; i < ob.maxSize; i++ {
		if ob.packets[i].data != nil {
			ob.pool.Put(ob.packets[i].data[:0])
			ob.packets[i] = bufferedPacket{}
		}
	}
	ob.notEmpty.Broadcast()
}
