package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/asticode/go-astiav"
)

type PCMFrame struct {
	// Interleaved s16le stereo 48k
	Data []byte
	// Start PTS in 48 kHz samples for the first sample in Data
	PTS48 int64
	// Number of samples per channel contained in Data
	NbSamples int
}

type PCMStreamer struct {
	fc           *astiav.FormatContext
	audioStream  *astiav.Stream
	decCtx       *astiav.CodecContext
	swr          *astiav.SoftwareResampleContext
	srcFrame     *astiav.Frame
	dstFrame     *astiav.Frame
	cancel       context.CancelFunc
	pr           *io.PipeReader
	pw           *io.PipeWriter
	writerClosed bool
	runOnce      sync.Once
	errMu        sync.Mutex
	runErr       error

	// target format
	targetRate    int
	targetLayout  astiav.ChannelLayout
	targetFormat  astiav.SampleFormat
	targetNbChans int

	// PTS mapping
	timeBase     astiav.Rational // stream time_base
	initedSWR    bool
	inRate       int
	inFmt        astiav.SampleFormat
	inLayout     astiav.ChannelLayout
	outPTS48Next int64 // media-time running cursor for next output sample at 48k
	gotFirstPTS  bool
	firstInPTS   int64
	firstPTS48   int64

	// FIFO of raw s16le 48k stereo samples (bytes)
	// Sample-aligned: len(fifo) is always a multiple of 4 bytes (2ch * 2 bytes)
	fifo []byte
}

func (s *PCMStreamer) initFIFO() {
	if s.fifo == nil {
		s.fifo = make([]byte, 0, 3840*8) // a few frames buffer
	}
}

// StartPCMStream: unchanged signature; internal changes to init pacing
func StartPCMStream(
	ctx context.Context,
	inputURL string,
	seek, to *int,
) (*PCMStreamer, error) {
	if inputURL == "" {
		return nil, fmt.Errorf("StartPCMStream: empty input URL")
	}
	if strings.Contains(inputURL, "youtube.com/watch") {
		return nil, fmt.Errorf("StartPCMStream: refusing to open webpage URL: %s", inputURL)
	}

	_ = astiav.GetLogLevel()

	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, errors.New("alloc format context")
	}

	dict := astiav.NewDictionary()
	defer dict.Free()
	_ = dict.Set("user_agent", "Mozilla/5.0 ...", 0)
	_ = dict.Set("referer", "https://www.youtube.com/", 0)
	_ = dict.Set("reconnect", "1", 0)
	_ = dict.Set("reconnect_streamed", "1", 0)
	_ = dict.Set("reconnect_delay_max", "5", 0)
	_ = dict.Set("rw_timeout", "15000000", 0)
	_ = dict.Set("http_persistent", "1", 0)
	_ = dict.Set("http_multiple", "0", 0)

	var inFmt *astiav.InputFormat
	isHLS := isManifestURL(inputURL)
	if isHLS {
		inFmt = astiav.FindInputFormat("hls")
		_ = dict.Set("allowed_extensions", "ALL", 0)
		_ = dict.Set("http_seekable", "0", 0)
		_ = dict.Set("live_start_index", "0", 0)
		// DO NOT set fflags=nobuffer; let demuxer pace live reasonably
		_ = dict.Set("probesize", "262144", 0)
		_ = dict.Set("analyzeduration", "2000000", 0)
		headers := "Origin: https://www.youtube.com\r\nAccept: */*\r\nConnection: keep-alive\r\n"
		_ = dict.Set("headers", headers, 0)
	}

	if err := fc.OpenInput(inputURL, inFmt, dict); err != nil {
		if isHLS {
			_ = dict.Set("live_start_index", "-1", 0)
			_ = dict.Set("analyzeduration", "4000000", 0)
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

	swr := astiav.AllocSoftwareResampleContext()
	if swr == nil {
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return nil, errors.New("alloc swr")
	}

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
		targetRate:    48000,
		targetLayout:  astiav.ChannelLayoutStereo,
		targetFormat:  astiav.SampleFormatS16,
		targetNbChans: 2,
		timeBase:      st.TimeBase(),
	}
	ps.initFIFO()

	go ps.run(ctx2, seek, to)
	return ps, nil
}

func (s *PCMStreamer) Close() {
	s.runOnce.Do(func() { s.cancel() })
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

func (s *PCMStreamer) Stdout() io.Reader { return s.pr }

func (s *PCMStreamer) run(ctx context.Context, seek, to *int) {
	defer func() {
		s.writerClosed = true
		_ = s.pw.Close()
	}()

	// Optional seek (unchanged logic but uses stream timebase)
	if seek != nil && *seek > 0 {
		tb := s.audioStream.TimeBase()
		ts := int64(float64(*seek) / tb.Float64())
		_ = s.fc.SeekFrame(s.audioStream.Index(), ts, astiav.NewSeekFlags())
		_ = s.fc.Flush()
	}

	packet := astiav.AllocPacket()
	defer packet.Free()

	// Optional soft stop (based on media PTS, not wall clock)
	var stopPTS48 int64 = -1
	if to != nil && *to > 0 {
		// end = seek + to; we’ll compute in 48k after anchor known
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
				_ = s.decCtx.SendPacket(nil)
				for {
					s.srcFrame.Unref()
					if err := s.decCtx.ReceiveFrame(s.srcFrame); err != nil {
						break
					}
					if err := s.onDecodedFrame(s.srcFrame); err != nil {
						s.setErr(err)
						return
					}
				}
				_ = s.flushSWR()
				return
			}
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
			if err := s.onDecodedFrame(s.srcFrame); err != nil {
				s.setErr(err)
				return
			}

			// Compute stop PTS after we know anchor
			if stopPTS48 < 0 && to != nil && *to > 0 && s.gotFirstPTS {
				// if seek is set we started at seek position, end = seek + to
				seekSec := 0
				if seek != nil {
					seekSec = *seek
				}
				stopPTS48 = s.firstPTS48 + int64((seekSec+*to)*48000)
			}
			if stopPTS48 >= 0 && s.outPTS48Next >= stopPTS48 {
				_ = s.flushSWR()
				return
			}
		}
	}
}

func (s *PCMStreamer) onDecodedFrame(src *astiav.Frame) error {
	// Capture input params from the first frame
	if !s.initedSWR {
		s.inRate = src.SampleRate()
		if s.inRate == 0 {
			s.inRate = s.decCtx.SampleRate()
			if s.inRate == 0 {
				s.inRate = 48000
			}
		}
		s.inFmt = src.SampleFormat()
		if s.inFmt.Name() == "" {
			s.inFmt = s.decCtx.SampleFormat()
			if s.inFmt.Name() == "" {
				s.inFmt = astiav.SampleFormatS16
			}
		}
		s.inLayout = src.ChannelLayout()
		if !s.inLayout.Valid() || s.inLayout.Channels() == 0 {
			s.inLayout = s.decCtx.ChannelLayout()
			if !s.inLayout.Valid() || s.inLayout.Channels() == 0 {
				s.inLayout = astiav.ChannelLayoutStereo
			}
		}

		// Set up SWR with explicit in/out and init
		// go-astiav provides SetOptions via frames or via context setters depending on version.
		// The ConvertFrame(src, dst) path will auto-config if both frames are fully specified.
		// We’ll use frames route but also call swr.Init() by doing a dummy convert to lock it in.

		s.dstFrame.Unref()
		s.dstFrame.SetChannelLayout(s.targetLayout)
		s.dstFrame.SetSampleRate(s.targetRate)
		s.dstFrame.SetSampleFormat(s.targetFormat)

		s.initedSWR = true
	}

	// Establish media-time anchor from first input PTS
	if !s.gotFirstPTS {
		inPTS := src.Pts()
		if inPTS == astiav.NoPtsValue {
			inPTS = 0
		}
		s.firstInPTS = inPTS
		// Map to 48k timeline
		s.firstPTS48 = astiav.RescaleQ(inPTS, s.timeBase, astiav.NewRational(1, 48000))
		s.outPTS48Next = s.firstPTS48
		s.gotFirstPTS = true
	}

	// Convert to 48k s16le stereo
	return s.convertAndWritePCM(src)
}

func (s *PCMStreamer) convertAndWritePCM(src *astiav.Frame) error {
	s.dstFrame.Unref()

	// Ensure src has the fields filled (defensive)
	if src.SampleRate() == 0 {
		src.SetSampleRate(s.inRate)
	}
	if src.SampleFormat().Name() == "" {
		src.SetSampleFormat(s.inFmt)
	}
	if !src.ChannelLayout().Valid() || src.ChannelLayout().Channels() == 0 {
		src.SetChannelLayout(s.inLayout)
	}

	// Estimate out samples using swr delay
	inNb := src.NbSamples()
	delay := s.swr.Delay(int64(s.inRate))
	outSamples := int(((delay+int64(inNb))*int64(s.targetRate) + int64(s.inRate-1)) / int64(s.inRate))
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

	b, err := s.dstFrame.Data().Bytes(0)
	if err != nil {
		return fmt.Errorf("dst bytes: %w", err)
	}
	// Append to FIFO
	s.fifo = append(s.fifo, b...)

	// While we have enough for one Opus frame (960 samples/ch => 960*ch*2 bytes)
	const frameBytes = 960 * 2 * 2
	for len(s.fifo) >= frameBytes {
		chunk := s.fifo[:frameBytes]
		// PTS48 for this chunk is current outPTS48Next
		pts := s.outPTS48Next
		// Emit PCMFrame as length-prefixed into pipe:
		if err := writePCMFrame(s.pw, PCMFrame{
			Data:      chunk,
			PTS48:     pts,
			NbSamples: 960,
		}); err != nil {
			return err
		}
		// Advance
		s.outPTS48Next += 960
		// pop
		s.fifo = s.fifo[frameBytes:]
	}
	return nil
}

// Flush any remaining samples in SWR, then drop tail < 960 samples to keep cadence strict
func (s *PCMStreamer) flushSWR() error {
	for {
		d := s.swr.Delay(int64(s.inRate))
		if d <= 0 {
			break
		}
		outSamples := int((d*int64(s.targetRate) + int64(s.inRate-1)) / int64(s.inRate))
		if outSamples <= 0 {
			break
		}
		s.dstFrame.Unref()
		s.dstFrame.SetNbSamples(outSamples)
		s.dstFrame.SetChannelLayout(s.targetLayout)
		s.dstFrame.SetSampleRate(s.targetRate)
		s.dstFrame.SetSampleFormat(s.targetFormat)
		if err := s.dstFrame.AllocBuffer(0); err != nil {
			return err
		}
		if err := s.swr.ConvertFrame(nil, s.dstFrame); err != nil {
			return err
		}
		b, err := s.dstFrame.Data().Bytes(0)
		if err != nil {
			return err
		}
		s.fifo = append(s.fifo, b...)
		// Drain full frames
		const frameBytes = 960 * 2 * 2
		for len(s.fifo) >= frameBytes {
			chunk := s.fifo[:frameBytes]
			pts := s.outPTS48Next
			if err := writePCMFrame(s.pw, PCMFrame{
				Data:      chunk,
				PTS48:     pts,
				NbSamples: 960,
			}); err != nil {
				return err
			}
			s.outPTS48Next += 960
			s.fifo = s.fifo[frameBytes:]
		}
	}
	// Drop tail < one frame to keep strict 20 ms pacing
	return nil
}

// Simple length-prefixed writer so the reader can recover frames and PTS
// Layout: [8 bytes PTS48][4 bytes NbSamples][4 bytes dataLen][data...]
func writePCMFrame(w io.Writer, f PCMFrame) error {
	var hdr [16]byte
	// PTS48
	putI64(hdr[0:8], f.PTS48)
	// NbSamples
	putI32(hdr[8:12], int32(f.NbSamples))
	// dataLen
	putI32(hdr[12:16], int32(len(f.Data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(f.Data)
	return err
}

func putI32(b []byte, v int32) {
	_ = b[3]
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func putI64(b []byte, v int64) {
	_ = b[7]
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
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
