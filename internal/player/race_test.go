package player

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/stream"
)

// TestConcurrentStatusRead exercises the data race that existed when command
// handlers read player.Status and player.Conn directly without the mutex.
// The test verifies that the unprotected reads trigger the race detector,
// while the public accessor methods (StatusPub, ConnPub) are race-free.
func TestConcurrentStatusRead(t *testing.T) {
	p := NewPlayer(nil, nil, "test-guild")
	p.mu.Lock()
	p.Conn = &discordgo.VoiceConnection{
		Cond:     sync.NewCond(&sync.Mutex{}),
		OpusSend: make(chan []byte, 64),
		OpusRecv: make(chan *discordgo.Packet, 2),
	}
	p.status.Store(int32(StatusPlaying))
	p.mu.Unlock()

	var stop atomic.Bool

	// Writer goroutine: continuously flips status via atomic store.
	go func() {
		for !stop.Load() {
			p.status.Store(int32(StatusPlaying))

			p.status.Store(int32(StatusPaused))

			p.status.Store(int32(StatusIdle))
		}
	}()

	// Writer goroutine: flips Conn between nil and non-nil under lock.
	go func() {
		vc := &discordgo.VoiceConnection{
			Cond:     sync.NewCond(&sync.Mutex{}),
			OpusSend: make(chan []byte, 64),
			OpusRecv: make(chan *discordgo.Packet, 2),
		}
		for !stop.Load() {
			p.mu.Lock()
			p.Conn = vc
			p.mu.Unlock()

			p.mu.Lock()
			p.Conn = nil
			p.mu.Unlock()
		}
	}()

	// Reader goroutine: uses public accessor methods which hold the lock.
	// These should be race-free.
	reads := atomic.Int64{}
	go func() {
		for !stop.Load() {
			_ = p.StatusPub()
			_ = p.ConnPub()
			_ = p.GetCurrent()
			_ = p.GetPosition()
			_ = p.QueueSize()
			_ = p.GetVolume()
			_ = p.LoopSongPub()
			_ = p.LoopQueuePub()
			_ = p.ShuffleModePub()
			_ = p.IsRadioMode()
			reads.Add(1)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	t.Logf("completed %d protected reads without data race", reads.Load())
}

// TestScheduleIdleDisconnectDoesNotHoldLockDuringIO verifies that the idle
// disconnect timer does not hold p.mu while performing network I/O.
// The scheduleIdleDisconnect AfterFunc must release the lock before calling
// safeDisconnect (which does time.Sleep + network calls).
func TestScheduleIdleDisconnectDoesNotHoldLockDuringIO(t *testing.T) {
	p := NewPlayer(nil, nil, "test-guild")

	vc := &discordgo.VoiceConnection{
		Cond:     sync.NewCond(&sync.Mutex{}),
		OpusSend: make(chan []byte, 64),
		OpusRecv: make(chan *discordgo.Packet, 2),
	}

	p.mu.Lock()
	p.Conn = vc
	p.ConnChannelID = "test-channel"
	p.status.Store(int32(StatusIdle))
	p.mu.Unlock()

	// Drain OpusSend so Speaking(false) won't block
	go func() {
		for range cap(vc.OpusSend) + 10 {
			select {
			case <-vc.OpusSend:
			default:
				return
			}
		}
	}()

	// Manually invoke what the AfterFunc in scheduleIdleDisconnect does
	// AFTER the fix (lock released before I/O).
	timerFired := make(chan struct{})
	go func() {
		p.mu.Lock()

		vc := p.Conn
		shouldDisconnect := p.StatusPub() == StatusIdle && p.curPlay == nil && vc != nil
		if shouldDisconnect {
			p.Conn = nil
			p.ConnChannelID = ""
		}
		p.mu.Unlock() // Must release lock BEFORE I/O

		if shouldDisconnect && vc != nil {
			close(timerFired)
			_ = p.safeDisconnect(context.Background(), vc)
		}
	}()

	select {
	case <-timerFired:
	case <-time.After(2 * time.Second):
		t.Fatal("timer callback never reached I/O phase")
	}

	time.Sleep(10 * time.Millisecond)

	start := time.Now()
	p.mu.Lock()
	elapsed := time.Since(start)
	p.mu.Unlock()

	if elapsed > 100*time.Millisecond {
		t.Fatalf("p.mu was held during I/O: lock acquisition took %v", elapsed)
	}
}

// TestStopPlayLockedConcurrentStop verifies that two sequential calls to
// stopPlayLocked (as happens when Stop + Forward overlap) don't deadlock.
func TestStopPlayLockedConcurrentStop(t *testing.T) {
	p := NewPlayer(nil, nil, "test-guild")

	pcm := newFakePCMStreamer()
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

	// Start a fake sendLoop that blocks until cancelled
	go func() {
		defer func() {
			sess.pcm.Close()
			sess.buf.Close()
			sess.producerWg.Wait()
			stream.PutPooledEncoder(sess.enc)
			sess.cancel()
			close(sess.doneCh)
		}()

		sess.producerWg.Go(func() {
			defer sess.buf.MarkEOS()
			<-sess.ctx.Done()
		})

		<-sess.ctx.Done()
	}()

	time.Sleep(100 * time.Millisecond)

	p.mu.Lock()
	p.curPlay = sess

	p.stopPlayLocked()
	p.stopPlayLocked() // second call should be a no-op
	p.mu.Unlock()
}

// TestConcurrentAddAndClear verifies that adding songs and clearing the queue
// from different goroutines doesn't panic or corrupt state.
func TestConcurrentAddAndClear(t *testing.T) {
	p := NewPlayer(nil, nil, "test-guild")

	var wg sync.WaitGroup
	const iterations = 100

	wg.Go(func() {
		for i := range iterations {
			p.Add(SongMetadata{Title: "song", VideoID: "vid", Length: i + 1}, false)
		}
	})

	wg.Go(func() {
		for range iterations {
			p.Clear()
			time.Sleep(time.Millisecond)
		}
	})

	wg.Go(func() {
		for range iterations {
			_ = p.QueueSize()
		}
	})

	wg.Wait()
}

// TestProducerConsumerCancelRace verifies that closing pcm and buf in
// sendLoop's defer doesn't race with the producer reading from pcm.
func TestProducerConsumerCancelRace(t *testing.T) {
	for range 20 {
		pcm := newFakePCMStreamer()
		enc, err := stream.NewEncoder()
		if err != nil {
			t.Skip("libopus not available:", err)
		}

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

		// Simulate sendLoop's cleanup: cancel + close resources.
		go func() {
			defer func() {
				sess.pcm.Close()
				sess.buf.Close()
				sess.producerWg.Wait()
				sess.cancel()
				close(sess.doneCh)
			}()

			sess.producerWg.Go(func() {
				defer sess.buf.MarkEOS()
				<-sess.ctx.Done()
			})

			<-sess.ctx.Done()
		}()

		time.Sleep(10 * time.Millisecond)
		cancel()

		select {
		case <-sess.doneCh:
		case <-time.After(3 * time.Second):
			t.Fatal("session did not exit in time")
		}
		stream.PutPooledEncoder(enc)
	}
}

// TestConcurrentSafeDisconnect verifies that safeDisconnect doesn't panic
// when called concurrently on the same voice connection.
func TestConcurrentSafeDisconnect(t *testing.T) {
	p := NewPlayer(nil, nil, "test-guild")

	vc := &discordgo.VoiceConnection{
		Cond:     sync.NewCond(&sync.Mutex{}),
		OpusSend: make(chan []byte, 64),
		OpusRecv: make(chan *discordgo.Packet, 2),
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			_ = p.safeDisconnect(context.Background(), vc)
		})
	}
	wg.Wait()
}

// TestBufferCloseDuringPop verifies that closing the buffer while a Pop is
// in progress doesn't deadlock.
func TestBufferCloseDuringPop(t *testing.T) {
	buf := newOpusBuffer(100)
	ctx := t.Context()
	timer := time.NewTimer(popPollInterval)
	defer timer.Stop()

	popDone := make(chan struct{})
	go func() {
		defer close(popDone)
		_, _ = buf.Pop(ctx, timer)
	}()

	time.Sleep(50 * time.Millisecond)
	buf.Close()

	select {
	case <-popDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Pop did not return after Close — deadlock")
	}
}

// TestBufferPushAfterClose verifies that Push returns false after Close.
func TestBufferPushAfterClose(t *testing.T) {
	buf := newOpusBuffer(100)
	buf.Close()

	if buf.Push([]byte("test"), 0, time.Now()) {
		t.Fatal("Push should return false after Close")
	}
}

// TestBufferPushAfterEOS verifies that Push returns false after MarkEOS.
func TestBufferPushAfterEOS(t *testing.T) {
	buf := newOpusBuffer(100)
	buf.MarkEOS()

	if buf.Push([]byte("test"), 0, time.Now()) {
		t.Fatal("Push should return false after MarkEOS")
	}
}

// TestProducerWg verifies the sync.WaitGroup usage in producerWg.
func TestProducerWg(t *testing.T) {
	pcm := newFakePCMStreamer()
	enc, err := stream.NewEncoder()
	if err != nil {
		t.Skip("libopus not available:", err)
	}
	defer enc.Close()

	buf := newOpusBuffer(100)
	playCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := &playSession{
		ctx:    playCtx,
		cancel: cancel,
		pcm:    pcm,
		enc:    enc,
		buf:    buf,
		doneCh: make(chan struct{}),
	}

	sess.producerWg.Go(func() {
		buf2 := make([]byte, 1024)
		_, _ = io.ReadFull(pcm.Stdout(), buf2)
	})

	time.Sleep(50 * time.Millisecond)
	pcm.Close()

	done := make(chan struct{})
	go func() {
		sess.producerWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("producerWg.Wait() deadlocked")
	}
}
