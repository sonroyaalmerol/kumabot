package player

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/stream"
)

type fakePCMStreamer struct {
	pr *io.PipeReader
	pw *io.PipeWriter
	ch chan stream.ReconnectSignal
}

func newFakePCMStreamer() *fakePCMStreamer {
	pr, pw := io.Pipe()
	return &fakePCMStreamer{pr: pr, pw: pw, ch: make(chan stream.ReconnectSignal, 1)}
}

func (f *fakePCMStreamer) Stdout() io.Reader                          { return f.pr }
func (f *fakePCMStreamer) ReconnectCh() <-chan stream.ReconnectSignal { return f.ch }
func (f *fakePCMStreamer) Close() {
	_ = f.pr.Close()
	_ = f.pw.Close()
}

func TestSendLoopExitsOnCancel(t *testing.T) {
	p := NewPlayer(nil, nil, nil, "test-guild")
	vc := &discordgo.VoiceConnection{
		OpusSend: make(chan []byte, 64),
		OpusRecv: make(chan *discordgo.Packet, 2),
	}

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

	go p.sendLoop(vc, nil, nil, 0, sess)

	// Give sendLoop time to start up
	time.Sleep(200 * time.Millisecond)

	// Cancel the session (simulating stop/skip)
	cancel()

	select {
	case <-sess.doneCh:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("sendLoop did not exit within 3s after cancellation - likely deadlock")
	}
}

func TestSendLoopExitsWhenProducerBlocked(t *testing.T) {
	p := NewPlayer(nil, nil, nil, "test-guild")
	vc := &discordgo.VoiceConnection{
		OpusSend: make(chan []byte, 64),
		OpusRecv: make(chan *discordgo.Packet, 2),
	}

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

	go p.sendLoop(vc, nil, nil, 0, sess)

	// Wait for producer to start and block reading from the pipe
	time.Sleep(200 * time.Millisecond)

	// Cancel while producer is blocked in readPCMFrame
	cancel()

	select {
	case <-sess.doneCh:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("sendLoop did not exit within 3s when producer was blocked - deadlock")
	}
}

func TestSendLoopNoGoroutineLeak(t *testing.T) {
	p := NewPlayer(nil, nil, nil, "test-guild")
	vc := &discordgo.VoiceConnection{
		OpusSend: make(chan []byte, 64),
		OpusRecv: make(chan *discordgo.Packet, 2),
	}

	// Let things settle
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := range 10 {
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

		go p.sendLoop(vc, nil, nil, 0, sess)
		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case <-sess.doneCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: sendLoop did not exit", i)
		}

		// Give defers time to finish
		time.Sleep(50 * time.Millisecond)
	}

	// Force GC to clean up any dead goroutines
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Allow a small tolerance for runtime goroutines
	if after > before+2 {
		t.Fatalf("goroutine leak detected: before=%d after=%d", before, after)
	}
}
