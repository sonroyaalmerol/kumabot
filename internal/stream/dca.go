package stream

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
)

// -------------------- DCAEncoder (Opus via FFmpeg/libopus) --------------------

type DCAEncoder struct {
	cc         *astiav.CodecContext
	frame      *astiav.Frame
	pkt        *astiav.Packet
	sampleRate int
	channels   int
	frameSize  int // samples per channel per frame (20ms at 48k -> 960)
	bw         *bufio.Writer
}

// NewDCAEncoder creates a stereo 48kHz Opus encoder tuned for ~160kbps.
func NewDCAEncoder(w io.Writer) (*DCAEncoder, error) {
	const (
		sampleRate = 48000
		channels   = 2
		frameSize  = 960 // 20ms at 48k
	)

	codec := astiav.FindEncoderByName("libopus")
	if codec == nil {
		return nil, errors.New("libopus encoder not found (FFmpeg built without libopus?)")
	}
	if !codec.IsEncoder() {
		return nil, errors.New("codec is not an encoder")
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, errors.New("alloc codec context")
	}

	cc.SetSampleRate(sampleRate)
	cc.SetChannelLayout(astiav.ChannelLayoutStereo) // 2 channels
	cc.SetSampleFormat(astiav.SampleFormatS16)      // input format
	cc.SetBitRate(160_000)                          // target ~160 kbps

	// Optional: enforce CBR-like behavior if desired via options dictionary
	// opts := astiav.NewDictionary()
	// defer opts.Free()
	// _ = opts.Set("vbr", "off", 0)       // CBR-like
	// _ = opts.Set("application", "audio", 0)
	// _ = opts.Set("compression_level", "8", 0)

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

	return &DCAEncoder{
		cc:         cc,
		frame:      frame,
		pkt:        pkt,
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  frameSize,
		bw:         bufio.NewWriterSize(w, 64*1024),
	}, nil
}

func (d *DCAEncoder) Close() {
	if d.pkt != nil {
		d.pkt.Free()
	}
	if d.frame != nil {
		d.frame.Free()
	}
	if d.cc != nil {
		d.cc.Free()
	}
}

// EncodePCMToDCA reads interleaved s16le stereo 48k PCM from r and writes
// DCA frames (2-byte len + Opus packet) to the writer supplied at NewDCAEncoder.
func (d *DCAEncoder) EncodePCMToDCA(r io.Reader) error {
	const bytesPerSample = 2
	frameBytes := d.frameSize * d.channels * bytesPerSample

	reader := bufio.NewReaderSize(r, 64*1024)
	pcmBuf := make([]byte, frameBytes)

	for {
		_, err := io.ReadFull(reader, pcmBuf)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// Flush encoder
				if err := d.flush(); err != nil {
					return err
				}
				_ = d.bw.Flush()
				return nil
			}
			return fmt.Errorf("read pcm: %w", err)
		}

		// Put PCM data into frame
		if err := d.frame.Data().SetBytes(pcmBuf, 0); err != nil {
			return fmt.Errorf("frame set bytes: %w", err)
		}

		// Send frame
		if err := d.cc.SendFrame(d.frame); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}

		// Receive all packets produced
		for {
			d.pkt.Unref()
			if err := d.cc.ReceivePacket(d.pkt); err != nil {
				if astErr, ok := err.(astiav.Error); ok && astErr.Is(astiav.ErrorAgain) {
					break
				}
				if astErr, ok := err.(astiav.Error); ok && astErr.Is(io.EOF) {
					break
				}
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("receive packet: %w", err)
			}

			n := d.pkt.Size()
			if n > 0xFFFF {
				return fmt.Errorf("opus packet too large: %d", n)
			}

			// DCA header + payload
			var hdr [2]byte
			binary.LittleEndian.PutUint16(hdr[:], uint16(n))
			if _, err := d.bw.Write(hdr[:]); err != nil {
				return fmt.Errorf("write dca len: %w", err)
			}
			if _, err := d.bw.Write(d.pkt.Data()); err != nil {
				return fmt.Errorf("write dca packet: %w", err)
			}
		}
	}
}

func (d *DCAEncoder) flush() error {
	// Signal EOF to encoder
	if err := d.cc.SendFrame(nil); err != nil {
		if astErr, ok := err.(astiav.Error); ok && astErr.Is(astiav.ErrorEOF) {
			return nil
		}
	}
	for {
		d.pkt.Unref()
		if err := d.cc.ReceivePacket(d.pkt); err != nil {
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrorAgain) || astErr.Is(astiav.ErrorEOF)) {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		n := d.pkt.Size()
		if n > 0xFFFF {
			return fmt.Errorf("opus packet too large: %d", n)
		}
		var hdr [2]byte
		binary.LittleEndian.PutUint16(hdr[:], uint16(n))
		if _, err := d.bw.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := d.bw.Write(d.pkt.Data()); err != nil {
			return err
		}
	}
	return nil
}
