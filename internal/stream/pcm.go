package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asticode/go-astiav"
)

type PCMStreamer struct {
	fc            *astiav.FormatContext
	audioStream   *astiav.Stream
	decCtx        *astiav.CodecContext
	swr           *astiav.SoftwareResampleContext
	srcFrame      *astiav.Frame
	dstFrame      *astiav.Frame
	cancel        context.CancelFunc
	pr            *io.PipeReader
	pw            *io.PipeWriter
	writerClosed  bool
	runOnce       sync.Once
	errMu         sync.Mutex
	runErr        error
	targetRate    int
	targetLayout  astiav.ChannelLayout
	targetFormat  astiav.SampleFormat
	targetNbChans int
	fifo          []byte
	frameBytes    int
}

func pcmDebugf(format string, args ...any) {
	if debugOn() {
		_, _ = fmt.Fprintf(os.Stderr, "[stream/pcm] "+format+"\n", args...)
	}
}

// StartPCMStream opens inputURL, optionally seeks/trims, and produces raw s16le
// stereo 48k PCM on the returned stream's Stdout() reader.
func StartPCMStream(
	ctx context.Context,
	inputURL string,
	seek, to *int, // seconds, seek applied before opening; to is a soft stop
) (*PCMStreamer, error) {
	if inputURL == "" {
		return nil, fmt.Errorf("StartPCMStream: empty input URL")
	}
	if strings.Contains(inputURL, "youtube.com/watch") {
		return nil, fmt.Errorf("StartPCMStream: refusing to open webpage URL: %s", inputURL)
	}

	_ = astiav.GetLogLevel() // ensure FFmpeg is initialized; optional

	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, errors.New("alloc format context")
	}

	// Handle reconnect and input options similar to CLI
	dict := astiav.NewDictionary()
	defer dict.Free()
	_ = dict.Set("user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36", 0)
	_ = dict.Set("referer", "https://www.youtube.com/", 0)
	_ = dict.Set("reconnect", "1", 0)
	_ = dict.Set("reconnect_streamed", "1", 0)
	_ = dict.Set("reconnect_delay_max", "5", 0)
	_ = dict.Set("rw_timeout", "15000000", 0)
	_ = dict.Set("http_persistent", "1", 0)
	_ = dict.Set("http_multiple", "0", 0)

	var inFmt *astiav.InputFormat
	isHLS := isManifestURL(inputURL)
	if debugOn() {
		kind := "DIRECT"
		if isHLS {
			kind = "HLS"
		}
		fmt.Fprintf(os.Stderr, "[stream/pcm] opening kind=%s url=%s\n", kind, inputURL)
	}

	if isHLS {
		// HLS-specific
		inFmt = astiav.FindInputFormat("hls")
		// Help the HLS demuxer accept nonstandard extensions and URL forms
		_ = dict.Set("allowed_extensions", "ALL", 0)
		_ = dict.Set("http_seekable", "0", 0)
		// For DVR/live HLS, start near live edge (-1) or at beginning (0)
		_ = dict.Set("live_start_index", "0", 0)
		_ = dict.Set("fflags", "nobuffer", 0)
		_ = dict.Set("probesize", "262144", 0)        // 256k
		_ = dict.Set("analyzeduration", "2000000", 0) // 2s

		// Hint demuxer not to attempt seeking
		_ = dict.Set("seekable", "0", 0)

		// _ = d.Set("protocol_whitelist", "file,http,https,tcp,tls,crypto", 0)
		headers := "Origin: https://www.youtube.com\r\nAccept: */*\r\nConnection: keep-alive\r\n"
		_ = dict.Set("headers", headers, 0)
		if debugOn() {
			_, _ = fmt.Fprintf(os.Stderr, "[stream/pcm] opening HLS: %s\n", inputURL)
		}
	} else {
		if debugOn() {
			_, _ = fmt.Fprintf(os.Stderr, "[stream/pcm] opening DIRECT: %s\n", inputURL)
		}
	}

	if err := fc.OpenInput(inputURL, inFmt, dict); err != nil {
		// One-time retry with a small delay and toggled start index for HLS
		if isHLS {
			pcmDebugf("OpenInput failed (%v), retrying once with live_start_index=-1", err)
			_ = dict.Set("live_start_index", "-1", 0)
			_ = dict.Set("analyzeduration", "4000000", 0) // 4s
			time.Sleep(250 * time.Millisecond)
			if err2 := fc.OpenInput(inputURL, inFmt, dict); err2 != nil {
				fc.Free()
				return nil, fmt.Errorf("open input retry failed: %w (first: %v)", err2, err)
			}
		} else {
			fc.Free()
			return nil, fmt.Errorf("open input: %w", err)
		}
	}

	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("find stream info: %w", err)
	}

	// Find best audio stream
	st, codec, err := fc.FindBestStream(astiav.MediaTypeAudio, -1, -1)
	if err != nil || st == nil || codec == nil {
		fc.CloseInput()
		fc.Free()
		if err != nil {
			return nil, fmt.Errorf("find best audio stream: %w", err)
		}
		return nil, errors.New("no audio stream found")
	}
	pcmDebugf("best audio stream index=%d codec=%s tb=%s", st.Index(), codec.Name(), st.TimeBase().String())

	decCtx := astiav.AllocCodecContext(codec)
	if decCtx == nil {
		fc.CloseInput()
		fc.Free()
		return nil, errors.New("alloc codec context")
	}
	if err := decCtx.FromCodecParameters(st.CodecParameters()); err != nil {
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("codec from params: %w", err)
	}
	decCtx.SetTimeBase(st.TimeBase())

	if err := decCtx.Open(codec, nil); err != nil {
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("open decoder: %w", err)
	}

	// Resampler to s16le stereo 48k
	targetRate := 48000
	targetLayout := astiav.ChannelLayoutStereo
	targetFormat := astiav.SampleFormatS16
	targetNbChans := targetLayout.Channels()

	swr := astiav.AllocSoftwareResampleContext()
	if swr == nil {
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return nil, errors.New("alloc swr")
	}
	// Set input params
	inLayout := decCtx.ChannelLayout()
	if !inLayout.Valid() || inLayout.Channels() == 0 {
		// Fall back to channel count from stream codec parameters
		ch := 0
		if p := st.CodecParameters(); p != nil {
			ch = p.Channels()
		}
		switch ch {
		case 1:
			inLayout = astiav.ChannelLayoutMono
		case 2:
			inLayout = astiav.ChannelLayoutStereo
		default:
			inLayout = astiav.ChannelLayout{}
		}
	}
	// SWR options are set via Options() on its Class; but go-astiav exposes a simpler API:
	// We will configure by creating temporary frames for src/dst.

	// Prepare frames
	srcFrame := astiav.AllocFrame()
	dstFrame := astiav.AllocFrame()
	if srcFrame == nil || dstFrame == nil {
		if srcFrame != nil {
			srcFrame.Free()
		}
		if dstFrame != nil {
			dstFrame.Free()
		}
		swr.Free()
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return nil, errors.New("alloc frames")
	}

	// Pipe for output PCM
	pr, pw := io.Pipe()

	ctx2, cancel := context.WithCancel(ctx)
	ps := &PCMStreamer{
		fc:            fc,
		audioStream:   st,
		decCtx:        decCtx,
		swr:           swr,
		srcFrame:      srcFrame,
		dstFrame:      dstFrame,
		cancel:        cancel,
		pr:            pr,
		pw:            pw,
		targetRate:    targetRate,
		targetLayout:  targetLayout,
		targetFormat:  targetFormat,
		targetNbChans: targetNbChans,
	}

	// Start background decode loop
	go ps.run(ctx2, seek, to, inLayout)

	return ps, nil
}

func (s *PCMStreamer) initFIFO() {
	if s.frameBytes == 0 {
		s.frameBytes = 960 * 2 * 2
	}
	if s.fifo == nil {
		s.fifo = make([]byte, 0, s.frameBytes*4) // small capacity for low latency
	}
}

func (s *PCMStreamer) Next20ms() ([]byte, error) {
	// This is a helper; in our integrated pipeline below we don't expose it directly.
	return nil, errors.New("not implemented in public API")
}

func (s *PCMStreamer) Stdout() io.Reader { return s.pr }

func (s *PCMStreamer) Close() {
	s.runOnce.Do(func() {
		s.cancel()
	})
	if s.pr != nil {
		_ = s.pr.Close()
	}
	if s.pw != nil && !s.writerClosed {
		_ = s.pw.Close()
	}
	if s.srcFrame != nil {
		s.srcFrame.Free()
	}
	if s.dstFrame != nil {
		s.dstFrame.Free()
	}
	if s.swr != nil {
		s.swr.Free()
	}
	if s.decCtx != nil {
		s.decCtx.Free()
	}
	if s.fc != nil {
		s.fc.CloseInput()
		s.fc.Free()
	}
}

func (s *PCMStreamer) run(ctx context.Context, seek, to *int, inLayout astiav.ChannelLayout) {
	defer func() {
		s.writerClosed = true
		_ = s.pw.Close()
	}()

	// Perform an actual seek if requested
	if seek != nil && *seek > 0 {
		// Convert seconds to stream timebase
		tb := s.audioStream.TimeBase()
		ts := int64(float64(*seek) / tb.Float64())
		// Seek to nearest keyframe before ts
		if err := s.fc.SeekFrame(s.audioStream.Index(), ts, astiav.NewSeekFlags()); err != nil {
			// Non-fatal; continue without seeking
		}
		_ = s.fc.Flush()
	}

	packet := astiav.AllocPacket()
	defer packet.Free()

	// Init SWR lazily once we get first decoded frame (to know input format/rate)
	swrInitialized := false

	startWall := time.Now()
	softStop := int64(-1)
	if to != nil && *to > 0 {
		softStop = int64(*to * 1000) // ms
	}

	for {
		select {
		case <-ctx.Done():
			s.setErr(ctx.Err())
			return
		default:
		}

		packet.Unref()
		if err := s.fc.ReadFrame(packet); err != nil {
			if astErr, ok := err.(astiav.Error); ok && astErr.Is(io.EOF) {
				// Flush decoder
				_ = s.decCtx.SendPacket(nil)
				for {
					s.srcFrame.Unref()
					if err := s.decCtx.ReceiveFrame(s.srcFrame); err != nil {
						break
					}
					if !swrInitialized {
						if err := s.initSWR(inLayout); err != nil {
							s.setErr(err)
							return
						}
						swrInitialized = true
					}
					if err := s.convertAndWritePCM(s.srcFrame); err != nil {
						s.setErr(err)
						return
					}
				}
				if swrInitialized {
					if err := s.drainSWR(); err != nil {
						s.setErr(err)
					}
				}
				return
			}
			// If it's not EOF, it might be EAGAIN, continue
			if astErr, ok := err.(astiav.Error); ok && astErr.Is(astiav.ErrEagain) {
				continue
			}
			s.setErr(fmt.Errorf("read frame: %w", err))
			return
		}

		if packet.StreamIndex() != s.audioStream.Index() {
			continue
		}

		if err := s.decCtx.SendPacket(packet); err != nil {
			// drop on EAGAIN
			if astErr, ok := err.(astiav.Error); !ok || !astErr.Is(astiav.ErrEagain) {
				s.setErr(fmt.Errorf("send packet: %w", err))
				return
			}
		}

		for {
			s.srcFrame.Unref()
			if err := s.decCtx.ReceiveFrame(s.srcFrame); err != nil {
				if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(io.EOF)) {
					break
				}
				s.setErr(fmt.Errorf("receive frame: %w", err))
				return
			}

			if !swrInitialized {
				if err := s.initSWR(inLayout); err != nil {
					s.setErr(err)
					return
				}
				swrInitialized = true
			}

			if err := s.convertAndWritePCM(s.srcFrame); err != nil {
				s.setErr(err)
				return
			}

			// Soft stop based on wall clock if -to provided (approximate)
			if softStop > 0 {
				elapsedMs := time.Since(startWall).Milliseconds()
				if elapsedMs >= softStop {
					return
				}
			}
		}
	}
}

func (s *PCMStreamer) initSWR(inLayout astiav.ChannelLayout) error {
	// Configure SWR via its Options
	inRate := s.decCtx.SampleRate()
	inChLayout := inLayout
	if !inChLayout.Valid() || inChLayout.Channels() == 0 {
		ch := 0
		if s.audioStream != nil && s.audioStream.CodecParameters() != nil {
			ch = s.audioStream.CodecParameters().Channels()
		}
		switch ch {
		case 1:
			inChLayout = astiav.ChannelLayoutMono
		case 2:
			inChLayout = astiav.ChannelLayoutStereo
		default:
			inChLayout = astiav.ChannelLayout{}
		}
	}

	if cls := s.swr.Class(); cls != nil {
		opts := cls.Options()
		// Note: Set expects string values; use .String() / fmt.Sprintf
		_ = opts.Set("in_channel_layout", inChLayout.String(), 0)
		_ = opts.Set("in_sample_rate", fmt.Sprintf("%d", inRate), 0)
		_ = opts.Set("in_sample_fmt", s.decCtx.SampleFormat().Name(), 0)

		_ = opts.Set("out_channel_layout", s.targetLayout.String(), 0)
		_ = opts.Set("out_sample_rate", fmt.Sprintf("%d", s.targetRate), 0)
		_ = opts.Set("out_sample_fmt", s.targetFormat.Name(), 0)
	}

	// Prepare static dst frame params; nb_samples will be set per conversion
	s.dstFrame.SetChannelLayout(s.targetLayout)
	s.dstFrame.SetSampleRate(s.targetRate)
	s.dstFrame.SetSampleFormat(s.targetFormat)
	return nil
}

func (s *PCMStreamer) convertAndWritePCM(src *astiav.Frame) error {
	s.dstFrame.Unref()
	inRate := s.decCtx.SampleRate()
	delay := s.swr.Delay(int64(inRate))
	inNb := src.NbSamples()
	outSamples := int(((delay+int64(inNb))*int64(s.targetRate) + int64(inRate-1)) / int64(inRate))
	if outSamples <= 0 {
		outSamples = 1
	}
	s.dstFrame.SetNbSamples(outSamples)
	s.dstFrame.SetChannelLayout(s.targetLayout)
	s.dstFrame.SetSampleRate(s.targetRate)
	s.dstFrame.SetSampleFormat(s.targetFormat)
	if err := s.dstFrame.AllocBuffer(0); err != nil {
		return fmt.Errorf("dst alloc buffer: %w", err)
	}

	if err := s.swr.ConvertFrame(src, s.dstFrame); err != nil {
		return fmt.Errorf("swr convert: %w", err)
	}

	if s.dstFrame.SampleFormat() != astiav.SampleFormatS16 ||
		s.dstFrame.ChannelLayout().Channels() != 2 ||
		s.dstFrame.SampleRate() != 48000 {
		pcmDebugf("unexpected dst params fmt=%s ch=%d sr=%d",
			s.dstFrame.SampleFormat().String(),
			s.dstFrame.ChannelLayout().Channels(),
			s.dstFrame.SampleRate())
	}

	// Get interleaved bytes from dst frame
	b, err := s.dstFrame.Data().Bytes(0)
	if err != nil {
		return fmt.Errorf("dst bytes: %w", err)
	}
	if debugOn() {
		// reuse encoder’s helper or copy here
		var sum float64
		for i := 0; i+1 < len(b); i += 2 {
			v := int16(uint16(b[i]) | (uint16(b[i+1]) << 8))
			sum += float64(v) * float64(v)
		}
		pcmDebugf("resampled bytes=%d mean-square=%f", len(b), sum/float64(len(b)/2))
	}
	_, err = s.pw.Write(b)
	return err
}

// drainSWR flushes remaining samples after EOF
func (s *PCMStreamer) drainSWR() error {
	inRate := s.decCtx.SampleRate()
	for {
		d := s.swr.Delay(int64(inRate))
		if d <= 0 {
			return nil
		}
		outSamples := int((d*int64(s.targetRate) + int64(inRate-1)) / int64(inRate))
		if outSamples <= 0 {
			return nil
		}
		s.dstFrame.Unref()
		s.dstFrame.SetNbSamples(outSamples)
		s.dstFrame.SetChannelLayout(s.targetLayout)
		s.dstFrame.SetSampleRate(s.targetRate)
		s.dstFrame.SetSampleFormat(s.targetFormat)
		if err := s.dstFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("drain alloc: %w", err)
		}
		if err := s.swr.ConvertFrame(nil, s.dstFrame); err != nil {
			return fmt.Errorf("swr drain convert: %w", err)
		}
		b, err := s.dstFrame.Data().Bytes(0)
		if err != nil {
			return fmt.Errorf("drain bytes: %w", err)
		}
		if len(b) == 0 {
			return nil
		}
		if _, err := s.pw.Write(b); err != nil {
			return err
		}
	}
}

func (s *PCMStreamer) setErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.runErr == nil {
		s.runErr = err
	}
}
