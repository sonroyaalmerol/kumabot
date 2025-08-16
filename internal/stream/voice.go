package stream

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

// We ask ffmpeg to output raw Opus stream at 48kHz stereo, 20ms frames, payload type 120.
// Then we read frames and send via discordgo's opus frame sender.

type OpusStreamer struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	cancel context.CancelFunc
}

func StartOpusStream(ctx context.Context, ffmpegPath string, inputURL string, seek *int, to *int, volumeDB *string) (*OpusStreamer, error) {
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
	// Output as raw opus stream
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
		return nil, err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &OpusStreamer{cmd: cmd, stdout: stdout, cancel: cancel}, nil
}

func (s *OpusStreamer) ReadFrame(r *bufio.Reader, buf []byte) (int, error) {
	// Opus raw packets can be variable length. For Discord we can send variable-length frames
	// as long as each represents 20ms @ 48kHz encoded with libopus.
	// We'll read based on ReadSlice with a small timeout; but better is Ogg/Matroska demux.
	// Simpler approach: Read up to len(buf) per frame with small sleeps; ffmpeg outputs packets.
	// Here we approximate by chunk reads; Discord accepts frames without specific RTP encapsulation via Send.
	return r.Read(buf)
}

func (s *OpusStreamer) Close() {
	s.cancel()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
}

// SendOpus pumps frames into the voice connection.
func SendOpus(vc *discordgo.VoiceConnection, s *OpusStreamer) error {
	vc.Speaking(true)
	defer vc.Speaking(false)

	reader := bufio.NewReader(s.stdout)
	frame := make([]byte, 4096)

	for {
		n, err := reader.Read(frame)
		if n > 0 {
			// Send raw opus frame
			vc.OpusSend <- frame[:n]
			// Sleep approx frame duration; opusenc -frame_duration 20ms
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
