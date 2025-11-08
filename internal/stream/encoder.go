package stream

import (
	"fmt"
	"os"

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

func debugOn() bool {
	v := os.Getenv("DEBUG")
	return v == "1" || v == "true" || v == "TRUE"
}

func dprintf(format string, args ...any) {
	if debugOn() {
		_, _ = fmt.Fprintf(os.Stderr, "[stream/encoder] "+format+"\n", args...)
	}
}

func pcmRMS16LE(b []byte) float64 {
	if len(b)%2 != 0 {
		return 0
	}
	var sum float64
	cnt := 0
	for i := 0; i < len(b); i += 2 {
		v := int16(uint16(b[i]) | (uint16(b[i+1]) << 8))
		sum += float64(v) * float64(v)
		cnt++
	}
	if cnt == 0 {
		return 0
	}
	return sum / float64(cnt) // mean square (no sqrt needed for debug)
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
		return nil, fmt.Errorf("libopus encoder not found (check ffmpeg installation)")
	}
	dprintf("using codec=%s (decoder=%v encoder=%v)", codec.Name(), codec.IsDecoder(), codec.IsEncoder())

	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, fmt.Errorf("failed to allocate codec context for libopus")
	}
	cc.SetSampleRate(sampleRate)
	cc.SetChannelLayout(astiav.ChannelLayoutStereo)
	cc.SetSampleFormat(astiav.SampleFormatS16)
	cc.SetBitRate(160_000)

	opts := astiav.NewDictionary()
	defer opts.Free()
	_ = opts.Set("frame_duration", "20", 0)
	_ = opts.Set("application", "audio", 0)

	if err := cc.Open(codec, opts); err != nil {
		cc.Free()
		return nil, fmt.Errorf("failed to open opus encoder (sr=%d ch=%d): %w", sampleRate, channels, err)
	}
	dprintf("opened encoder: sr=%d ch=%d fmt=%s bitrate=%d",
		cc.SampleRate(), cc.ChannelLayout().Channels(), cc.SampleFormat().String(), cc.BitRate())

	frame := astiav.AllocFrame()
	if frame == nil {
		cc.Free()
		return nil, fmt.Errorf("failed to allocate audio frame for encoder")
	}
	frame.SetSampleRate(sampleRate)
	frame.SetChannelLayout(astiav.ChannelLayoutStereo)
	frame.SetSampleFormat(astiav.SampleFormatS16)
	frame.SetNbSamples(frameSize)
	if err := frame.AllocBuffer(0); err != nil {
		frame.Free()
		cc.Free()
		return nil, fmt.Errorf("failed to allocate frame buffer: %w", err)
	}
	dprintf("prepared frame buffer: nb_samples=%d bytes_per_frame=%d",
		frameSize, frameSize*channels*2)

	pkt := astiav.AllocPacket()
	if pkt == nil {
		frame.Free()
		cc.Free()
		return nil, fmt.Errorf("failed to allocate packet for encoder")
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
	dprintf("closing encoder")
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
	expected := e.frameSize * e.channels * 2
	if len(pcm) != expected {
		return fmt.Errorf("invalid PCM frame size: expected %d bytes (960 samples * 2 channels * 2 bytes), got %d", expected, len(pcm))
	}

	if err := e.frame.Data().SetBytes(pcm, 0); err != nil {
		return fmt.Errorf("failed to set frame data bytes: %w", err)
	}
	if err := e.cc.SendFrame(e.frame); err != nil {
		return fmt.Errorf("failed to send frame to encoder: %w", err)
	}

	for {
		e.packet.Unref()
		if err := e.cc.ReceivePacket(e.packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEof)) {
				break
			}
			return fmt.Errorf("failed to receive opus packet: %w", err)
		}
		if err := onPacket(e.packet.Data()); err != nil {
			return fmt.Errorf("packet handler error: %w", err)
		}
	}
	return nil
}

func (e *Encoder) Flush(onPacket OpusPacketHandler) error {
	dprintf("flushing encoder")
	if err := e.cc.SendFrame(nil); err != nil {
		if astErr, ok := err.(astiav.Error); ok && astErr.Is(astiav.ErrEof) {
			return nil
		}
		return fmt.Errorf("failed to send flush frame: %w", err)
	}
	for {
		e.packet.Unref()
		if err := e.cc.ReceivePacket(e.packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEof)) {
				break
			}
			return fmt.Errorf("failed to receive packet during flush: %w", err)
		}
		dprintf("flush packet: size=%d", e.packet.Size())
		if err := onPacket(e.packet.Data()); err != nil {
			return fmt.Errorf("packet handler error during flush: %w", err)
		}
	}
	return nil
}

func (e *Encoder) FrameBytes() int {
	return e.frameSize * e.channels * 2
}
