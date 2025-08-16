package stream

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"gopkg.in/hraban/opus.v2"
)

type DCAEncoder struct {
	enc        *opus.Encoder
	sampleRate int
	channels   int
	frameSize  int // samples per channel per frame (20 ms -> 960 at 48k)
}

// NewDCAEncoder creates a stereo 48kHz Opus encoder tuned for music.
func NewDCAEncoder() (*DCAEncoder, error) {
	const (
		sampleRate = 48000
		channels   = 2
		frameSize  = 960 // 20ms at 48k
	)

	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppAudio)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}

	// Match your target ~160kbps
	if err := enc.SetBitrate(160_000); err != nil {
		return nil, fmt.Errorf("set bitrate: %w", err)
	}
	// Optional tuning
	// _ = enc.SetComplexity(8)       // 0..10
	// _ = enc.SetInBandFEC(true)     // if you want FEC for loss concealment
	// _ = enc.SetDTX(false)          // keep always-on
	// _ = enc.SetMaxBandwidth(opus.Fullband)

	return &DCAEncoder{
		enc:        enc,
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  frameSize,
	}, nil
}

// EncodePCMToDCA reads interleaved s16le stereo 48kHz PCM from r,
// encodes 20ms frames to Opus, and writes DCA frames (uint16 LE len + packet)
// to w. It stops cleanly on EOF.
func (d *DCAEncoder) EncodePCMToDCA(r io.Reader, w io.Writer) error {
	const bytesPerSample = 2
	frameBytes := d.frameSize * d.channels * bytesPerSample

	reader := bufio.NewReaderSize(r, 64*1024)
	pcmBytes := make([]byte, frameBytes)
	pcmInt16 := make([]int16, d.frameSize*d.channels)

	// Opus packets are typically < 1500 bytes, but allocate a safe buffer.
	outBuf := make([]byte, 4096)

	for {
		if _, err := io.ReadFull(reader, pcmBytes); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read pcm: %w", err)
		}

		// Convert little-endian bytes -> int16
		for i := 0; i < len(pcmInt16); i++ {
			j := i * 2
			// little-endian: low byte then high byte
			pcmInt16[i] = int16(uint16(pcmBytes[j]) | uint16(pcmBytes[j+1])<<8)
		}

		// Encode one 20ms frame
		n, err := d.enc.Encode(pcmInt16, outBuf)
		if err != nil {
			return fmt.Errorf("opus encode: %w", err)
		}
		packet := outBuf[:n]

		if n > 0xFFFF {
			return fmt.Errorf("opus packet too large: %d", n)
		}

		// DCA framing: 2-byte little-endian length + packet bytes
		var hdr [2]byte
		binary.LittleEndian.PutUint16(hdr[:], uint16(n))
		if _, err := w.Write(hdr[:]); err != nil {
			return fmt.Errorf("write dca len: %w", err)
		}
		if _, err := w.Write(packet); err != nil {
			return fmt.Errorf("write dca packet: %w", err)
		}
	}
}
