package player

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sonroyaalmerol/kumabot/internal/stream"
)

type blockingPCMStreamer struct {
	pr *io.PipeReader
	pw *io.PipeWriter
	ch chan stream.ReconnectSignal
}

func newBlockingPCMStreamer() *blockingPCMStreamer {
	pr, pw := io.Pipe()
	return &blockingPCMStreamer{pr: pr, pw: pw, ch: make(chan stream.ReconnectSignal, 1)}
}

func (b *blockingPCMStreamer) Stdout() io.Reader                          { return b.pr }
func (b *blockingPCMStreamer) ReconnectCh() <-chan stream.ReconnectSignal { return b.ch }
func (b *blockingPCMStreamer) Close() {
	_ = b.pr.Close()
	_ = b.pw.Close()
}

func TestStopPlayLockedDoesNotDeadlock(t *testing.T) {
	p := NewPlayer(nil, nil, nil, "test-guild")

	pcm := newBlockingPCMStreamer()
	enc, err := stream.NewEncoder()
	if err != nil {
		t.Skip("libopus not available:", err)
	}
	defer enc.Close()

	buf := newOpusBuffer(100)
	playCtx, cancel := context.WithCancel(context.Background())
	sess := &playSession{
		ctx:    playCtx,
		cancel: cancel,
		pcm:    pcm,
		enc:    enc,
		buf:    buf,
		doneCh: make(chan struct{}),
	}

	// Start a fake sendLoop that blocks in consumePackets because the buffer is empty
	// and the producer is blocked reading from the pipe.
	go func() {
		// simulate what sendLoop does: wait for producer, then close doneCh
		defer func() {
			sess.pcm.Close()
			sess.buf.Close()
			sess.producerWg.Wait()
			close(sess.doneCh)
		}()

		sess.producerWg.Go(func() {
			defer sess.buf.MarkEOS()
			// Block reading from pipe
			_ = make([]byte, 1024)
			_, _ = io.ReadFull(sess.pcm.Stdout(), make([]byte, 16))
		})

		// consumePackets would block in Pop because buffer is empty
		// and producer is blocked.
		<-sess.ctx.Done()
	}()

	// Give the goroutine time to block
	time.Sleep(100 * time.Millisecond)

	p.mu.Lock()
	p.curPlay = sess

	// This should unblock quickly because stopPlayLocked now closes pcm and buf
	start := time.Now()
	p.stopPlayLocked()
	elapsed := time.Since(start)
	p.mu.Unlock()

	if elapsed > 1*time.Second {
		t.Fatalf("stopPlayLocked took too long: %v (possible deadlock)", elapsed)
	}
}
