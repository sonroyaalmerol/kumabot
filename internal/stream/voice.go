package stream

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type OpusStreamer struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *bytes.Buffer
	cancel context.CancelFunc
}

func StartOpusStream(
	ctx context.Context,
	ffmpegPath string,
	inputURL string,
	seek *int,
	to *int,
	volumeDB *string,
) (*OpusStreamer, error) {
	ctx2, cancel := context.WithCancel(ctx)

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
		"-i", inputURL,
		"-vn",
		"-ac", "2",
		"-ar", "48000",
	}
	if seek != nil {
		args = append(args, "-ss", strconv.Itoa(*seek))
	}
	if to != nil {
		args = append(args, "-to", strconv.Itoa(*to))
	}
	if volumeDB != nil && *volumeDB != "" {
		args = append(args, "-filter:a", "volume="+*volumeDB)
	}
	// Output as raw opus stream: 48kHz stereo, 20ms packets
	args = append(args,
		"-c:a", "libopus",
		"-b:a", "160k",
		"-frame_duration", "20",
		"-application", "audio",
		"-f", "opus",
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx2, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg start: %w (stderr: %s)", err, stderr.String())
	}
	return &OpusStreamer{cmd: cmd, stdout: stdout, stderr: stderr, cancel: cancel}, nil
}

func (s *OpusStreamer) Close() {
	s.cancel()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
}

func (s *OpusStreamer) Stderr() string {
	if s.stderr == nil {
		return ""
	}
	return s.stderr.String()
}

// SendOpus pumps frames into the voice connection.
// It assumes ffmpeg is producing correctly framed Opus packets at 20 ms intervals.
// We still protect against blocking and early EOF.
func SendOpus(vc *discordgo.VoiceConnection, s *OpusStreamer) error {
	// Wait until the connection is ready (up to a few seconds)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (vc == nil || !vc.Ready) {
		time.Sleep(100 * time.Millisecond)
	}
	if vc == nil || !vc.Ready {
		return fmt.Errorf("voice connection not ready")
	}

	if err := vc.Speaking(true); err != nil {
		// Not fatal, but good to know
	}
	defer vc.Speaking(false)

	reader := bufio.NewReaderSize(s.stdout, 4096)

	// 4 KiB is fine for a single Opus packet at 20ms 160kbps; adjust if needed.
	frameBuf := make([]byte, 4096)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Read a packet. We can't rely on newline or separator; ffmpeg writes raw packets.
		// strategy: read whatever is available up to buffer size. If zero bytes available,
		// perform a small sleep via the ticker to approximate 20ms pacing.
		n, err := reader.Read(frameBuf)
		if n > 0 {
			// Non-blocking send to avoid deadlocks if vc stops
			select {
			case vc.OpusSend <- frameBuf[:n]:
			case <-time.After(200 * time.Millisecond):
				// If we can't send within 200ms, likely connection not accepting frames
				// Exit with error so caller can advance / reconnect
				return fmt.Errorf("opus send timed out")
			}
		}

		if err != nil {
			if err == io.EOF {
				// Include stderr for diagnostics
				st := s.Stderr()
				if strings.TrimSpace(st) != "" {
					return fmt.Errorf("ffmpeg ended: %s", st)
				}
				return nil
			}
			return err
		}

		// Pace approximately 20ms per frame
		<-ticker.C
	}
}
