package stream

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

type OpusPacketHandler func(pkt []byte) error

type Encoder struct {
	cc         *astiav.CodecContext
	frame      *astiav.Frame
	packet     *astiav.Packet
	sampleRate int
	channels   int
	frameSize  int // samples per channel for 20 ms at 48 kHz = 960
}

// NewEncoder creates an Opus encoder (libopus) at 48k stereo ~160kbps.
func NewEncoder() (*Encoder, error) {
	const (
		sampleRate = 48000
		channels   = 2
		frameSize  = 960
	)

	codec := astiav.FindEncoderByName("libopus")
	if codec == nil {
		return nil, errors.New("libopus encoder not found")
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, errors.New("alloc codec context")
	}
	cc.SetSampleRate(sampleRate)
	cc.SetChannelLayout(astiav.ChannelLayoutStereo)
	cc.SetSampleFormat(astiav.SampleFormatS16)
	cc.SetBitRate(160_000)

	if err := cc.Open(codec, nil); err != nil {
		cc.Free()
		return nil, fmt.Errorf("open opus encoder: %w", err)
	}

	frame := astiav.AllocFrame()
	if frame == nil {
		cc.Free()
		return nil, errors.New("alloc frame")
	}
	frame.SetSampleRate(sampleRate)
	frame.SetChannelLayout(astiav.ChannelLayoutStereo)
	frame.SetSampleFormat(astiav.SampleFormatS16)
	frame.SetNbSamples(frameSize)
	if err := frame.AllocBuffer(0); err != nil {
		frame.Free()
		cc.Free()
		return nil, fmt.Errorf("frame alloc buffer: %w", err)
	}

	pkt := astiav.AllocPacket()
	if pkt == nil {
		frame.Free()
		cc.Free()
		return nil, errors.New("alloc packet")
	}

	return &Encoder{
		cc:         cc,
		frame:      frame,
		packet:     pkt,
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  frameSize,
	}, nil
}

func (e *Encoder) Close() {
	if e.packet != nil {
		e.packet.Free()
	}
	if e.frame != nil {
		e.frame.Free()
	}
	if e.cc != nil {
		e.cc.Free()
	}
}

// EncodeFrame expects interleaved s16le PCM for exactly 960 samples/ch (20 ms).
// pcm length must be 960 * channels * 2 bytes = 3840 bytes.
func (e *Encoder) EncodeFrame(pcm []byte, onPacket OpusPacketHandler) error {
	if len(pcm) != e.frameSize*e.channels*2 {
		return fmt.Errorf("pcm must be %d bytes, got %d",
			e.frameSize*e.channels*2, len(pcm))
	}

	// Put PCM into frame (no copy API copies into AVFrame buffer)
	if err := e.frame.Data().SetBytes(pcm, 0); err != nil {
		return fmt.Errorf("frame set bytes: %w", err)
	}

	if err := e.cc.SendFrame(e.frame); err != nil {
		return fmt.Errorf("send frame: %w", err)
	}

	for {
		e.packet.Unref()
		if err := e.cc.ReceivePacket(e.packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEof)) {
				break
			}
			return fmt.Errorf("receive packet: %w", err)
		}
		// Pass raw Opus payload
		if err := onPacket(e.packet.Data()); err != nil {
			return err
		}
	}
	return nil
}

func (e *Encoder) Flush(onPacket OpusPacketHandler) error {
	if err := e.cc.SendFrame(nil); err != nil {
		if astErr, ok := err.(astiav.Error); ok && astErr.Is(astiav.ErrEof) {
			return nil
		}
	}
	for {
		e.packet.Unref()
		if err := e.cc.ReceivePacket(e.packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEof)) {
				break
			}
			return err
		}
		if err := onPacket(e.packet.Data()); err != nil {
			return err
		}
	}
	return nil
}

func (e *Encoder) FrameBytes() int {
	return e.frameSize * e.channels * 2
}
