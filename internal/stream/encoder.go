package stream

import (
	"errors"
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
		return nil, errors.New("libopus encoder not found")
	}
	dprintf("using codec=%s (decoder=%v encoder=%v)", codec.Name(), codec.IsDecoder(), codec.IsEncoder())

	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, errors.New("alloc codec context")
	}
	cc.SetSampleRate(sampleRate)
	cc.SetChannelLayout(astiav.ChannelLayoutStereo)
	cc.SetSampleFormat(astiav.SampleFormatS16)
	cc.SetBitRate(160_000)

	opts := astiav.NewDictionary()
	defer opts.Free()
	_ = opts.Set("vbr", "off", 0)           // debug: force CBR-like behavior to avoid DTX
	_ = opts.Set("application", "audio", 0) // better for music/general audio
	_ = opts.Set("compression_level", "8", 0)

	if err := cc.Open(codec, opts); err != nil {
		cc.Free()
		return nil, fmt.Errorf("open opus encoder: %w", err)
	}
	dprintf("opened encoder: sr=%d ch=%d fmt=%s bitrate=%d",
		cc.SampleRate(), cc.ChannelLayout().Channels(), cc.SampleFormat().String(), cc.BitRate())

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
	dprintf("prepared frame buffer: nb_samples=%d bytes_per_frame=%d",
		frameSize, frameSize*channels*2)

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
		return fmt.Errorf("pcm must be %d bytes, got %d", expected, len(pcm))
	}

	if err := e.frame.Data().SetBytes(pcm, 0); err != nil {
		return fmt.Errorf("frame set bytes: %w", err)
	}
	if debugOn() {
		ms := pcmRMS16LE(pcm)
		dprintf("frame PCM mean-square=%f", ms)
	}
	if err := e.cc.SendFrame(e.frame); err != nil {
		return fmt.Errorf("send frame: %w", err)
	}
	dprintf("sent frame: bytes=%d", len(pcm))

	for {
		e.packet.Unref()
		if err := e.cc.ReceivePacket(e.packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEof)) {
				break
			}
			return fmt.Errorf("receive packet: %w", err)
		}
		dprintf("got opus packet: size=%d", e.packet.Size())
		if err := onPacket(e.packet.Data()); err != nil {
			return err
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
	}
	for {
		e.packet.Unref()
		if err := e.cc.ReceivePacket(e.packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEof)) {
				break
			}
			return err
		}
		dprintf("flush packet: size=%d", e.packet.Size())
		if err := onPacket(e.packet.Data()); err != nil {
			return err
		}
	}
	return nil
}

func (e *Encoder) FrameBytes() int {
	return e.frameSize * e.channels * 2
}
