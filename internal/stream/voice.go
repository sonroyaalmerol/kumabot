package stream

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	ffmpeg "github.com/u2takey/ffmpeg-go"

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
	inputURL string,
	seek, to *int,
	volumeDB *string,
) (*OpusStreamer, error) {
	ctx2, cancel := context.WithCancel(ctx)

	// Build input with reconnect options
	in := ffmpeg.Input(
		inputURL,
		ffmpeg.KwArgs{
			"reconnect":           "1",
			"reconnect_streamed":  "1",
			"reconnect_delay_max": "5",
		},
	)

	// Start with audio stream; insert volume if requested
	stream := in.Audio()

	// Output options to match your original CLI
	outKw := ffmpeg.KwArgs{
		"vn":             "",        // -vn
		"ac":             "2",       // -ac 2
		"ar":             "48000",   // -ar 48000
		"c:a":            "libopus", // -c:a libopus
		"b:a":            "160k",    // -b:a 160k
		"frame_duration": "20",      // -frame_duration 20
		"application":    "audio",   // -application audio
		"f":              "opus",    // -f opus
	}

	// Accurate seek (after -i): put ss/to on output side
	if seek != nil {
		outKw["ss"] = fmt.Sprint(*seek)
	}
	if to != nil {
		outKw["to"] = fmt.Sprint(*to)
	}

	// Build graph to write to stdout
	node := stream.
		Output("pipe:", outKw).
		GlobalArgs("-hide_banner", "-loglevel", "error")

	// Compile to *exec.Cmd and hook up pipes
	cmd := node.Compile()
	// tie to our context by killing process on cancel
	go func() {
		<-ctx2.Done()
		_ = cmd.Process.Kill()
	}()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg start: %w (stderr: %s)", err, stderr.String())
	}

	return &OpusStreamer{
		cmd:    cmd,
		stdout: stdout,
		stderr: &stderr,
		cancel: cancel,
	}, nil
}

func (s *OpusStreamer) Close() {
	s.cancel()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
}

func SendOpus(vc *discordgo.VoiceConnection, s *OpusStreamer) error {
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

	reader := bufio.NewReaderSize(s.stdout, 4096)
	frame := make([]byte, 4096)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		n, err := reader.Read(frame)
		if n > 0 {
			select {
			case vc.OpusSend <- frame[:n]:
			case <-time.After(200 * time.Millisecond):
				return fmt.Errorf("opus send timeout")
			}
		}
		if err != nil {
			if err == io.EOF {
				st := s.stderr.String()
				if strings.TrimSpace(st) != "" {
					return fmt.Errorf("ffmpeg ended: %s", st)
				}
				return nil
			}
			return err
		}
		<-ticker.C
	}
}
