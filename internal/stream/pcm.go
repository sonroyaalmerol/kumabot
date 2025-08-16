package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

type PCMStreamer struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *bytes.Buffer
	cancel context.CancelFunc
}

func StartPCMStream(
	ctx context.Context,
	inputURL string,
	seek, to *int, // seconds, applied before -i for fast seek
) (*PCMStreamer, error) {
	ctx2, cancel := context.WithCancel(ctx)

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
	}
	if seek != nil {
		args = append(args, "-ss", fmt.Sprint(*seek))
	}
	if to != nil {
		args = append(args, "-to", fmt.Sprint(*to))
	}
	args = append(args, "-i", inputURL)
	args = append(args, "-vn", "-ac", "2", "-ar", "48000")
	args = append(args, "-f", "s16le", "pipe:1")

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

func (s *PCMStreamer) Stdout() io.Reader { return s.stdout }

func (s *PCMStreamer) Close() {
	s.cancel()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
}
