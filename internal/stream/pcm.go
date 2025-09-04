package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	pr, pw        io.PipeReader
	writerClosed  bool
	runOnce       sync.Once
	errMu         sync.Mutex
	runErr        error
	targetRate    int
	targetLayout  astiav.ChannelLayout
	targetFormat  astiav.SampleFormat
	targetNbChans int
}

// StartPCMStream opens inputURL, optionally seeks/trims, and produces raw s16le
// stereo 48k PCM on the returned stream's Stdout() reader.
func StartPCMStream(
	ctx context.Context,
	inputURL string,
	seek, to *int, // seconds, seek applied before opening; to is a soft stop
) (*PCMStreamer, error) {
	_ = astiav.GetLogLevel() // ensure FFmpeg is initialized; optional

	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, errors.New("alloc format context")
	}

	// Handle reconnect and input options similar to CLI
	dict := astiav.NewDictionary()
	defer dict.Free()
	// For network inputs, you can set options on the input format ctx
	// like reconnect flags. The exact keys depend on protocol/demuxer.
	// Example for HTTP(S):
	_ = dict.Set("reconnect", "1", 0)
	_ = dict.Set("reconnect_streamed", "1", 0)
	_ = dict.Set("reconnect_delay_max", "5", 0)

	// Pre-input seek: best effort for formats supporting it
	if seek != nil && *seek > 0 {
		// For input options, prefer "start_time" or "seekable" demuxer options if any.
		// Many demuxers ignore this; we'll also perform an actual SeekFrame later.
	}

	if err := fc.OpenInput(inputURL, nil, dict); err != nil {
		fc.Free()
		return nil, fmt.Errorf("open input: %w", err)
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
		// Some codecs don't set layout; derive from channels if needed
		// Fallback: use default layout for channel count
		switch decCtx.ChannelLayout().Channels() {
		case 1:
			inLayout = astiav.ChannelLayoutMono
		case 2:
			inLayout = astiav.ChannelLayoutStereo
		default:
			// Let SWR infer from channel count; layout compare may still be OK
			inLayout = decCtx.ChannelLayout()
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
		pr:            *pr,
		pw:            *pw,
		targetRate:    targetRate,
		targetLayout:  targetLayout,
		targetFormat:  targetFormat,
		targetNbChans: targetNbChans,
	}

	// Start background decode loop
	go ps.run(ctx2, seek, to, inLayout)

	return ps, nil
}

func (s *PCMStreamer) Stdout() io.Reader { return &s.pr }

func (s *PCMStreamer) Close() {
	s.runOnce.Do(func() {
		s.cancel()
	})
	_ = s.pr.Close()
	if !s.writerClosed {
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
				return
			}
			// If it's not EOF, it might be EAGAIN, continue
			if astErr, ok := err.(astiav.Error); ok && astErr.Is(astiav.ErrorAgain) {
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
			if astErr, ok := err.(astiav.Error); !ok || !astErr.Is(astiav.ErrorAgain) {
				s.setErr(fmt.Errorf("send packet: %w", err))
				return
			}
		}

		for {
			s.srcFrame.Unref()
			if err := s.decCtx.ReceiveFrame(s.srcFrame); err != nil {
				if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrorAgain) || astErr.Is(io.EOF)) {
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
	// Configure dst frame format
	s.dstFrame.SetChannelLayout(s.targetLayout)
	s.dstFrame.SetSampleRate(s.targetRate)
	s.dstFrame.SetSampleFormat(s.targetFormat)
	// We'll allocate buffers per convert call based on input nb_samples.

	// Configure SWR via its Options interface is not needed; go-astiav exposes direct API.
	// Here we rely on ConvertFrame(src,dst) configuring from frame params.

	// Some SWR builds expect explicit initialization; in go-astiav, ConvertFrame
	// will initialize as needed. If desired, you can set options on s.swr.Class().
	_ = s.swr.Class()

	return nil
}

func (s *PCMStreamer) convertAndWritePCM(src *astiav.Frame) error {
	// Prepare dst frame with same nb_samples scaled to target rate
	// SWR may output different number of samples; we allocate per call.
	s.dstFrame.Unref()
	s.dstFrame.SetNbSamples(src.NbSamples())
	s.dstFrame.SetChannelLayout(s.targetLayout)
	s.dstFrame.SetSampleRate(s.targetRate)
	s.dstFrame.SetSampleFormat(s.targetFormat)
	if err := s.dstFrame.AllocBuffer(0); err != nil {
		return fmt.Errorf("dst alloc buffer: %w", err)
	}

	if err := s.swr.ConvertFrame(src, s.dstFrame); err != nil {
		return fmt.Errorf("swr convert: %w", err)
	}

	// Get interleaved bytes from dst frame
	b, err := s.dstFrame.Data().Bytes(0)
	if err != nil {
		return fmt.Errorf("dst bytes: %w", err)
	}
	_, err = s.pw.Write(b)
	return err
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
