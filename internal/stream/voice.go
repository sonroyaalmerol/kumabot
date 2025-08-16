package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/bwmarrin/discordgo"
	"layeh.com/gopus"
)

// PCMStreamer manages ffmpeg PCM streaming (s16le, 48k, stereo).
type PCMStreamer struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *bytes.Buffer
	cancel context.CancelFunc
}

func StartPCMStream(
	ctx context.Context,
	inputURL string,
	seek, to *int, // seconds; use input-side seek like your Node sample
) (*PCMStreamer, error) {
	ctx2, cancel := context.WithCancel(ctx)

	args := []string{
		"-hide_banner", "-loglevel", "error",
		// Robust network options:
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
	}
	// Fast seek before input (like your Node code which set input options)
	if seek != nil {
		args = append(args, "-ss", fmt.Sprint(*seek))
	}
	if to != nil {
		args = append(args, "-to", fmt.Sprint(*to))
	}

	args = append(args, "-i", inputURL)

	// Audio only to PCM s16le, 48k stereo
	args = append(args,
		"-vn",
		"-ac", "2",
		"-ar", "48000",
	)

	// Output raw PCM
	args = append(args,
		"-f", "s16le",
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx2, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg start: %w (stderr: %s)", err, stderr.String())
	}

	return &PCMStreamer{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
		cancel: cancel,
	}, nil
}

func (s *PCMStreamer) Stdout() io.Reader {
	return s.stdout
}

func (s *PCMStreamer) Close() {
	s.cancel()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
}

// DCAEncoder encodes PCM frames to Opus and writes DCA framing to an io.Writer.
type DCAEncoder struct {
	enc        *gopus.Encoder
	sampleRate int
	channels   int
	frameSize  int // samples per channel per frame (20 ms -> 960 at 48k)
}

func NewDCAEncoder() (*DCAEncoder, error) {
	const (
		sampleRate = 48000
		channels   = 2
		frameSize  = 960 // 20ms at 48k
	)
	enc, err := gopus.NewEncoder(sampleRate, channels, gopus.Audio)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	// Tune bitrate as you like (matches your 160k setting)
	enc.SetBitrate(160000)

	// Optional: set complexity, VBR, etc., as needed
	return &DCAEncoder{
		enc:        enc,
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  frameSize,
	}, nil
}

// EncodePCMToDCA reads PCM from r and writes DCA-framed Opus packets to w.
// PCM must be interleaved s16le stereo at 48kHz.
func (d *DCAEncoder) EncodePCMToDCA(r io.Reader, w io.Writer) error {
	// Each 20ms frame is 960 samples/ch * 2 ch * 2 bytes = 3840 bytes
	const bytesPerSample = 2
	frameBytes := d.frameSize * d.channels * bytesPerSample
	pcmBuf := make([]byte, frameBytes)
	shorts := make([]int16, d.frameSize*d.channels)

	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		if _, err := io.ReadFull(reader, pcmBuf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read pcm: %w", err)
		}
		// Convert bytes to int16 little-endian
		for i := 0; i < len(shorts); i++ {
			j := i * 2
			shorts[i] = int16(pcmBuf[j]) | int16(int8(pcmBuf[j+1]))<<8
		}
		// Encode one 20ms frame
		packet, err := d.enc.Encode(shorts, d.frameSize, 4000)
		if err != nil {
			return fmt.Errorf("opus encode: %w", err)
		}
		// DCA framing: write uint16 length (LE), then packet bytes
		if len(packet) > 0xFFFF {
			return fmt.Errorf("opus packet too large: %d", len(packet))
		}
		var hdr [2]byte
		binary.LittleEndian.PutUint16(hdr[:], uint16(len(packet)))
		if _, err := w.Write(hdr[:]); err != nil {
			return fmt.Errorf("write dca len: %w", err)
		}
		if _, err := w.Write(packet); err != nil {
			return fmt.Errorf("write dca packet: %w", err)
		}
	}
}

// SendDCAPackets reads DCA frames (len + packet) from r and sends each packet
// to vc.OpusSend, paced at 20ms, like the discordgo example.
func SendDCAPackets(vc *discordgo.VoiceConnection, r io.Reader) error {
	// Wait until ready
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (vc == nil || !vc.Ready) {
		time.Sleep(100 * time.Millisecond)
	}
	if vc == nil || !vc.Ready {
		return fmt.Errorf("voice connection not ready")
	}

	_ = vc.Speaking(true)
	defer vc.Speaking(false)

	br := bufio.NewReaderSize(r, 32*1024)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var hdr [2]byte
	for {
		// Read length
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read dca len: %w", err)
		}
		n := binary.LittleEndian.Uint16(hdr[:])
		if n == 0 {
			<-ticker.C
			continue
		}
		pkt := make([]byte, int(n))
		if _, err := io.ReadFull(br, pkt); err != nil {
			return fmt.Errorf("read dca packet: %w", err)
		}

		<-ticker.C
		select {
		case vc.OpusSend <- pkt:
		case <-time.After(200 * time.Millisecond):
			return fmt.Errorf("opus send timeout")
		}
	}
}
