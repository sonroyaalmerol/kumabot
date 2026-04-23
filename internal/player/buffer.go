package player

import (
	"context"
	"sync"
	"time"
)

const popPollInterval = 20 * time.Millisecond

// typedPool is a generic, type-safe pool that avoids sync.Pool's interface boxing.
type typedPool[T any] struct {
	mu    sync.Mutex
	items []T
	newFn func() T
}

func (p *typedPool[T]) Get() T {
	p.mu.Lock()
	if len(p.items) == 0 {
		p.mu.Unlock()
		return p.newFn()
	}
	v := p.items[len(p.items)-1]
	p.items = p.items[:len(p.items)-1]
	p.mu.Unlock()
	return v
}

func (p *typedPool[T]) Put(v T) {
	p.mu.Lock()
	p.items = append(p.items, v)
	p.mu.Unlock()
}

type opusBuffer struct {
	mu       sync.Mutex
	packets  []bufferedPacket
	maxSize  int
	readPos  int
	writePos int
	closed   bool
	eos      bool
	pool     typedPool[[]byte]
}

type bufferedPacket struct {
	data     []byte
	pts48    int64
	targetTS time.Time
}

func newOpusBuffer(maxPackets int) *opusBuffer {
	return &opusBuffer{
		packets: make([]bufferedPacket, maxPackets),
		maxSize: maxPackets,
		pool: typedPool[[]byte]{
			newFn: func() []byte { return make([]byte, 0, 200) },
		},
	}
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
	buf := ob.pool.Get()
	buf = append(buf[:0], data...)

	ob.packets[ob.writePos] = bufferedPacket{
		data:     buf,
		pts48:    pts48,
		targetTS: targetTS,
	}
	ob.writePos = (ob.writePos + 1) % ob.maxSize
	return true
}

func (ob *opusBuffer) Pop(ctx context.Context) (bufferedPacket, bool) {
	for {
		ob.mu.Lock()
		if ob.closed {
			ob.mu.Unlock()
			return bufferedPacket{}, false
		}

		used := (ob.writePos - ob.readPos + ob.maxSize) % ob.maxSize
		if used > 0 {
			pkt := ob.packets[ob.readPos]
			ob.packets[ob.readPos] = bufferedPacket{}
			ob.readPos = (ob.readPos + 1) % ob.maxSize
			ob.mu.Unlock()
			return pkt, true
		}

		if ob.eos {
			ob.mu.Unlock()
			return bufferedPacket{}, false
		}
		ob.mu.Unlock()

		timer := time.NewTimer(popPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return bufferedPacket{}, false
		case <-timer.C:
		}
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
}
