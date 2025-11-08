package stream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asticode/go-astiav"
)

type PCMFrame struct {
	Data      []byte
	PTS48     int64
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

	// target
	targetRate    int
	targetLayout  astiav.ChannelLayout
	targetFormat  astiav.SampleFormat
	targetNbChans int

	timeBase     astiav.Rational
	initedSWR    bool // after first successful ConvertFrame
	inRate       int
	inLayout     astiav.ChannelLayout
	outPTS48Next int64
	gotFirstPTS  bool
	firstPTS48   int64
	inFmt        astiav.SampleFormat

	fifo []byte

	inputURL string
	isHLS    bool

	reconnectCh chan ReconnectSignal

	// crossfade state
	resumeAt48        int64 // if >=0, trim decoded audio until this sample index (48k)
	lastProducedPTS48 int64
	producedMu        sync.Mutex
}

type ReconnectSignal struct {
	LastSentPTS48 int64
}

var debugOnce int32

func pcmDebugf(format string, args ...any) {
	if debugOn() {
		_, _ = fmt.Fprintf(os.Stderr, "[stream/pcm] "+format+"\n", args...)
	}
}

func init() {
	if debugOn() {
		astiav.SetLogLevel(astiav.LogLevelDebug)
	}
}

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

	var inFmt *astiav.InputFormat
	isHLS := isManifestURL(inputURL)
	if isHLS {
		inFmt = astiav.FindInputFormat("hls")
		_ = dict.Set("allowed_extensions", "ALL", 0)
		_ = dict.Set("http_seekable", "0", 0)
		_ = dict.Set("live_start_index", "0", 0)
		_ = dict.Set("probesize", "262144", 0)
		_ = dict.Set("analyzeduration", "2000000", 0)
	}

	if err := fc.OpenInput(inputURL, inFmt, dict); err != nil {
		if isHLS {
			_ = dict.Set("live_start_index", "-1", 0)
			_ = dict.Set("analyzeduration", "4000000", 0)
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
		fifo:          make([]byte, 0, 3840*8),
		inputURL:      inputURL,
		isHLS:         isHLS,
		resumeAt48:    -1,
		reconnectCh:   make(chan ReconnectSignal, 1),
	}

	go ps.run(ctx2, seek, to)
	return ps, nil
}

func (s *PCMStreamer) ReconnectCh() <-chan ReconnectSignal {
	return s.reconnectCh
}

func (s *PCMStreamer) Stdout() io.Reader { return s.pr }

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

func (s *PCMStreamer) run(ctx context.Context, seek, to *int) {
	defer func() {
		s.writerClosed = true
		_ = s.pw.Close()
	}()

	if seek != nil && *seek > 0 {
		tb := s.audioStream.TimeBase()
		ts := int64(float64(*seek) / tb.Float64())
		_ = s.fc.SeekFrame(s.audioStream.Index(), ts, astiav.NewSeekFlags())
		_ = s.fc.Flush()
	}

	packet := astiav.AllocPacket()
	defer packet.Free()

	var stopPTS48 int64 = -1

	retry := 0
	const maxRetry = 3
	for {
		select {
		case <-ctx.Done():
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
						return
					}
				}
				_ = s.flushSWR()

				const frameBytes = 960 * 2 * 2
				if rem := len(s.fifo); rem > 0 {
					pad := make([]byte, frameBytes-rem) // zeros = silence
					s.fifo = append(s.fifo, pad...)
					chunk := s.fifo[:frameBytes]
					pts := s.outPTS48Next
					_ = s.writePCMFrameTracked(PCMFrame{
						Data:      chunk,
						PTS48:     pts,
						NbSamples: 960,
					})
					s.outPTS48Next += 960
					s.fifo = s.fifo[frameBytes:]
				}
				return
			}
			// Retryable network error?
			if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(astiav.ErrEio) || astErr.Is(astiav.ErrEtimedout)) {
				// fallthrough to retry path
			} else {
				// Some tls/http errors are not mapped; check string
				es := fmt.Sprint(err)
				if strings.Contains(es, "Connection reset by peer") ||
					strings.Contains(es, "The specified session has been invalidated") ||
					strings.Contains(es, "IO error") ||
					strings.Contains(es, "Input/output error") {
					// retry path
				} else {
					pcmDebugf("read frame error (fatal): %v", err)
					return
				}
			}
			if retry >= maxRetry {
				pcmDebugf("read frame error: %v (giving up after %d retries)", err, retry)
				return
			}
			retry++
			backoff := time.Duration(retry*300) * time.Millisecond
			pcmDebugf("read frame error: %v (retry %d/%d after %v)", err, retry, maxRetry, backoff)
			time.Sleep(backoff)
			if err := s.reopenAndSeek(); err != nil {
				pcmDebugf("reopen failed: %v", err)
				return
			}
			// Clear decoder buffered frames
			continue
		}
		retry = 0

		if packet.StreamIndex() != s.audioStream.Index() {
			continue
		}
		if err := s.decCtx.SendPacket(packet); err != nil {
			if astErr, ok := err.(astiav.Error); !ok || !astErr.Is(astiav.ErrEagain) {
				pcmDebugf("send packet error: %v", err)
				return
			}
		}
		for {
			s.srcFrame.Unref()
			if err := s.decCtx.ReceiveFrame(s.srcFrame); err != nil {
				if astErr, ok := err.(astiav.Error); ok && (astErr.Is(astiav.ErrEagain) || astErr.Is(io.EOF)) {
					break
				}
				pcmDebugf("receive frame error: %v", err)
				return
			}
			if err := s.onDecodedFrame(s.srcFrame); err != nil {
				pcmDebugf("onDecodedFrame error: %v", err)
				return
			}
			if stopPTS48 < 0 && to != nil && *to > 0 && s.gotFirstPTS {
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

// reopenAndSeek attempts to reopen the input and seek near the last emitted PTS
func (s *PCMStreamer) reopenAndSeek() error {
	s.producedMu.Lock()
	targetPTS := s.lastProducedPTS48
	s.producedMu.Unlock()

	if targetPTS <= 0 {
		targetPTS = s.outPTS48Next
	}

	pcmDebugf("reopenAndSeek: lastProduced=%d outNext=%d using=%d",
		s.lastProducedPTS48, s.outPTS48Next, targetPTS)

	// Close existing contexts
	if s.fc != nil {
		s.fc.CloseInput()
		s.fc.Free()
		s.fc = nil
	}

	// Alloc fresh format context
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return errors.New("alloc format context (reopen)")
	}
	dict := astiav.NewDictionary()
	var inFmt *astiav.InputFormat
	if s.isHLS {
		inFmt = astiav.FindInputFormat("hls")
		_ = dict.Set("allowed_extensions", "ALL", 0)
		_ = dict.Set("http_seekable", "0", 0)
		_ = dict.Set("live_start_index", "0", 0)
		_ = dict.Set("probesize", "262144", 0)
		_ = dict.Set("analyzeduration", "2000000", 0)
	}
	if err := fc.OpenInput(s.inputURL, inFmt, dict); err != nil {
		dict.Free()
		fc.Free()
		return fmt.Errorf("open input (reopen): %w", err)
	}
	dict.Free()
	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("find stream info (reopen): %w", err)
	}

	st, codec, err := fc.FindBestStream(astiav.MediaTypeAudio, -1, -1)
	if err != nil || st == nil || codec == nil {
		fc.CloseInput()
		fc.Free()
		if err != nil {
			return fmt.Errorf("find best audio stream (reopen): %w", err)
		}
		return errors.New("no audio stream found (reopen)")
	}

	if s.decCtx != nil {
		s.decCtx.Free()
	}
	decCtx := astiav.AllocCodecContext(codec)
	if decCtx == nil {
		fc.CloseInput()
		fc.Free()
		return errors.New("alloc codec context (reopen)")
	}
	if err := decCtx.FromCodecParameters(st.CodecParameters()); err != nil {
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("codec from params (reopen): %w", err)
	}
	decCtx.SetTimeBase(st.TimeBase())
	if err := decCtx.Open(codec, nil); err != nil {
		decCtx.Free()
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("open decoder (reopen): %w", err)
	}

	s.fc = fc
	s.audioStream = st
	s.decCtx = decCtx
	s.timeBase = st.TimeBase()

	// Seek with larger safety margin to ensure we get audio before target
	const safetyMarginSamples = 9600 // ~200ms at 48kHz - larger margin for network jitter
	safeSeekTarget := targetPTS - safetyMarginSamples
	if safeSeekTarget < 0 {
		safeSeekTarget = 0
	}

	ts := astiav.RescaleQ(safeSeekTarget, astiav.NewRational(1, 48000), s.timeBase)
	if err := s.fc.SeekFrame(s.audioStream.Index(), ts, astiav.NewSeekFlags(astiav.SeekFlagBackward)); err != nil {
		pcmDebugf("seek failed: %v (continuing anyway)", err)
	}
	_ = s.fc.Flush()

	pcmDebugf("reopenAndSeek: target=%d safeSeek=%d (-%d samples)",
		targetPTS, safeSeekTarget, safetyMarginSamples)

	// Clear FIFO and reset SWR
	s.fifo = s.fifo[:0]
	s.initedSWR = false
	s.inRate = 0
	s.inLayout = astiav.ChannelLayout{}

	// Set exact resume point - this ensures we trim to exactly where we left off
	s.resumeAt48 = targetPTS

	// Signal reconnection - DON'T send the PTS, let consumer continue naturally
	select {
	case s.reconnectCh <- ReconnectSignal{LastSentPTS48: targetPTS}:
	default:
	}

	pcmDebugf("reconnection prepared: will resume at PTS48=%d", targetPTS)
	return nil
}

func (s *PCMStreamer) onDecodedFrame(src *astiav.Frame) error {
	// Read definitive input params from frame, fallback to decoder context
	if s.inRate == 0 {
		s.inRate = src.SampleRate()
		if s.inRate == 0 {
			s.inRate = s.decCtx.SampleRate()
			if s.inRate == 0 {
				s.inRate = 48000
			}
		}
	}
	if s.inFmt.Name() == "" {
		s.inFmt = src.SampleFormat()
		if s.inFmt.Name() == "" {
			s.inFmt = s.decCtx.SampleFormat()
			if s.inFmt.Name() == "" {
				s.inFmt = astiav.SampleFormatS16
			}
		}
	}
	if !s.inLayout.Valid() || s.inLayout.Channels() == 0 {
		s.inLayout = src.ChannelLayout()
		if !s.inLayout.Valid() || s.inLayout.Channels() == 0 {
			s.inLayout = s.decCtx.ChannelLayout()
			if !s.inLayout.Valid() || s.inLayout.Channels() == 0 {
				s.inLayout = astiav.ChannelLayoutStereo
			}
		}
	}

	// First PTS anchor
	if !s.gotFirstPTS {
		inPTS := src.Pts()
		if inPTS == astiav.NoPtsValue {
			inPTS = 0
		}
		s.firstPTS48 = astiav.RescaleQ(inPTS, s.timeBase, astiav.NewRational(1, 48000))
		s.outPTS48Next = s.firstPTS48
		s.gotFirstPTS = true
	}

	// Ensure fields are set on src for SWR
	if src.SampleRate() == 0 {
		src.SetSampleRate(s.inRate)
	}
	if !src.ChannelLayout().Valid() || src.ChannelLayout().Channels() == 0 {
		src.SetChannelLayout(s.inLayout)
	}

	// Prepare dst frame common params
	s.dstFrame.Unref()
	s.dstFrame.SetChannelLayout(s.targetLayout)
	s.dstFrame.SetSampleRate(s.targetRate)
	s.dstFrame.SetSampleFormat(s.targetFormat)

	if !s.initedSWR {
		// First convert: do a conservative outSamples estimate without Delay()
		inNb := src.NbSamples()
		if inNb <= 0 {
			inNb = 1024
		}
		outSamples := int((int64(inNb)*int64(s.targetRate) + int64(s.inRate-1)) / int64(s.inRate))
		if outSamples <= 0 {
			outSamples = 1
		}
		s.dstFrame.SetNbSamples(outSamples)
		if err := s.dstFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("dst alloc (init): %w", err)
		}
		// This first ConvertFrame initializes SWR internally using src/dst params
		if err := s.swr.ConvertFrame(src, s.dstFrame); err != nil {
			return fmt.Errorf("swr init convert: %w", err)
		}
		// Ensure SWR output is either S16 (packed) or S16P (planar)
		dfmt := s.dstFrame.SampleFormat()
		if dfmt != astiav.SampleFormatS16 && dfmt != astiav.SampleFormatS16P {
			return fmt.Errorf("unexpected dst sample format after swr init: %s", dfmt.String())
		}
		// Discard data from the init convert to avoid duplicating samples
		s.dstFrame.Unref()
		s.initedSWR = true
		return nil
	}

	// Normal convert after init: we can now use Delay() safely
	return s.convertAndWritePCM(src)
}

func (s *PCMStreamer) convertAndWritePCM(src *astiav.Frame) error {
	// Ensure src fields are sane
	if src.SampleRate() == 0 {
		src.SetSampleRate(s.inRate)
	}
	if !src.ChannelLayout().Valid() || src.ChannelLayout().Channels() == 0 {
		src.SetChannelLayout(s.inLayout)
	}

	inNb := src.NbSamples()
	if inNb <= 0 {
		return nil
	}

	// Safe to use Delay() after first init-convert
	delay := s.swr.Delay(int64(s.inRate))
	outSamples := int(((delay+int64(inNb))*int64(s.targetRate) + int64(s.inRate-1)) / int64(s.inRate))
	if outSamples <= 0 {
		outSamples = 1
	}
	// Defensive cap
	if outSamples > (inNb+2048)*3 {
		outSamples = (inNb + 2048) * 3
	}

	// Prepare dst frame for S16LE stereo 48k
	s.dstFrame.Unref()
	s.dstFrame.SetNbSamples(outSamples)
	s.dstFrame.SetChannelLayout(s.targetLayout) // Stereo
	s.dstFrame.SetSampleRate(s.targetRate)      // 48000
	s.dstFrame.SetSampleFormat(s.targetFormat)  // S16 (little-endian)
	if err := s.dstFrame.AllocBuffer(0); err != nil {
		return fmt.Errorf("dst alloc buffer: %w", err)
	}

	// Convert using SWR
	if err := s.swr.ConvertFrame(src, s.dstFrame); err != nil {
		return fmt.Errorf("swr convert: %w", err)
	}

	// Validate dst params
	nb := s.dstFrame.NbSamples()
	if nb <= 0 {
		return nil
	}
	if s.dstFrame.SampleRate() != 48000 ||
		s.dstFrame.ChannelLayout().Channels() != 2 ||
		s.dstFrame.SampleFormat() != astiav.SampleFormatS16 {
		return fmt.Errorf("unexpected dst params fmt=%s rate=%d ch=%d",
			s.dstFrame.SampleFormat().String(),
			s.dstFrame.SampleRate(),
			s.dstFrame.ChannelLayout().Channels())
	}

	// Build interleaved S16LE buffer based on declared sample format
	const bytesPerSample = 2
	ch := s.dstFrame.ChannelLayout().Channels()
	isPlanar := s.dstFrame.SampleFormat().IsPlanar()
	total := nb * ch * bytesPerSample
	interleaved := make([]byte, total)

	if !isPlanar {
		// Packed: copy with SamplesCopyToBuffer to respect linesize
		n, err := s.dstFrame.SamplesCopyToBuffer(interleaved, 1)
		if err != nil {
			return fmt.Errorf("packed copy to buffer: %w", err)
		}
		if n != total {
			return fmt.Errorf("packed copy size mismatch: got %d want %d", n, total)
		}
	} else {
		// Planar: interleave using linesizes and bytes-per-sample
		// We assume S16P as target; validate sample format
		if s.dstFrame.SampleFormat() != astiav.SampleFormatS16P {
			return fmt.Errorf("unexpected planar format: %s", s.dstFrame.SampleFormat().String())
		}
		// Gather plane byte slices
		planes := make([][]byte, ch)
		for c := 0; c < ch; c++ {
			pb, err := s.dstFrame.Data().Bytes(c)
			if err != nil {
				return fmt.Errorf("dst plane%d bytes: %w", c, err)
			}
			if len(pb) < nb*bytesPerSample {
				return fmt.Errorf("planar dst too small: ch%d=%d need=%d", c, len(pb), nb*bytesPerSample)
			}
			planes[c] = pb
		}
		// Interleave sample-by-sample, channel order as provided
		// sample index i: for each channel c, append 2 bytes
		outOff := 0
		for i := 0; i < nb; i++ {
			for c := 0; c < ch; c++ {
				src := planes[c][i*bytesPerSample : i*bytesPerSample+bytesPerSample]
				copy(interleaved[outOff:outOff+bytesPerSample], src)
				outOff += bytesPerSample
			}
		}
	}

	// Append to FIFO and emit exact 960-sample frames
	const frameBytes = 960 * 2 * 2
	s.fifo = append(s.fifo, interleaved...)

	for len(s.fifo) >= frameBytes {
		pts := s.outPTS48Next

		// Sample-accurate trim logic on resume
		if s.resumeAt48 >= 0 {
			frameStart := pts
			frameEnd := pts + 960

			pcmDebugf("trim check: frame=[%d..%d) resume=%d", frameStart, frameEnd, s.resumeAt48)

			// Case 1: Frame before resume point - skip
			if frameEnd <= s.resumeAt48 {
				pcmDebugf("  -> skipping frame (before resume point)")
				s.outPTS48Next += 960
				s.fifo = s.fifo[frameBytes:]
				continue
			}

			// Case 2: Resume point inside frame - trim
			if frameStart < s.resumeAt48 && s.resumeAt48 < frameEnd {
				offsetSamples := int(s.resumeAt48 - frameStart)
				offsetBytes := offsetSamples * 4

				pcmDebugf("  -> trimming %d samples (%d bytes) from frame start", offsetSamples, offsetBytes)

				partialLen := frameBytes - offsetBytes
				partial := make([]byte, partialLen)
				copy(partial, s.fifo[offsetBytes:frameBytes])

				s.fifo = s.fifo[frameBytes:]
				s.fifo = append(partial, s.fifo...)

				s.outPTS48Next = s.resumeAt48
				s.resumeAt48 = -1

				pcmDebugf("  -> resume aligned at PTS48=%d", s.outPTS48Next)
				continue
			}

			// Case 3: Frame at or after resume - aligned
			pcmDebugf("  -> frame aligned, resuming normal playback")
			s.resumeAt48 = -1
		}

		// Normal emission - NO CROSSFADE
		chunk := make([]byte, frameBytes)
		copy(chunk, s.fifo[:frameBytes])
		s.fifo = s.fifo[frameBytes:]

		if err := s.writePCMFrameTracked(PCMFrame{
			Data:      chunk,
			PTS48:     pts,
			NbSamples: 960,
		}); err != nil {
			return err
		}
		s.outPTS48Next += 960
	}
	return nil
}

func (s *PCMStreamer) flushSWR() error {
	if !s.initedSWR {
		return nil
	}
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

		// Interleave like convertAndWritePCM using declared format
		nb := s.dstFrame.NbSamples()
		if nb <= 0 {
			continue
		}
		const bytesPerSample = 2
		ch := s.targetLayout.Channels()
		total := nb * ch * bytesPerSample
		interleaved := make([]byte, total)

		if !s.dstFrame.SampleFormat().IsPlanar() {
			n, err := s.dstFrame.SamplesCopyToBuffer(interleaved, 1)
			if err != nil {
				return err
			}
			if n != total {
				return fmt.Errorf("flush packed copy size mismatch: got %d want %d", n, total)
			}
		} else {
			if s.dstFrame.SampleFormat() != astiav.SampleFormatS16P {
				return fmt.Errorf("flush unexpected planar format: %s", s.dstFrame.SampleFormat().String())
			}
			planes := make([][]byte, ch)
			for c := 0; c < ch; c++ {
				pb, err := s.dstFrame.Data().Bytes(c)
				if err != nil {
					return err
				}
				if len(pb) < nb*bytesPerSample {
					return fmt.Errorf("flush planar too small: ch%d=%d need=%d", c, len(pb), nb*bytesPerSample)
				}
				planes[c] = pb
			}
			outOff := 0
			for i := 0; i < nb; i++ {
				for c := 0; c < ch; c++ {
					src := planes[c][i*bytesPerSample : i*bytesPerSample+bytesPerSample]
					copy(interleaved[outOff:outOff+bytesPerSample], src)
					outOff += bytesPerSample
				}
			}
		}

		const frameBytes = 960 * 2 * 2
		s.fifo = append(s.fifo, interleaved...)
		for len(s.fifo) >= frameBytes {
			pts := s.outPTS48Next
			if s.resumeAt48 >= 0 {
				if pts+960 <= s.resumeAt48 {
					s.outPTS48Next += 960
					s.fifo = s.fifo[frameBytes:]
					continue
				}
				if pts < s.resumeAt48 && s.resumeAt48 < pts+960 {
					offSamp := int(s.resumeAt48 - pts)
					byteOff := offSamp * 4
					partial := make([]byte, frameBytes-byteOff)
					copy(partial, s.fifo[byteOff:frameBytes])
					s.fifo = s.fifo[frameBytes:]
					s.fifo = append(partial, s.fifo...)
					s.outPTS48Next = s.resumeAt48
					s.resumeAt48 = -1
					continue
				}
				s.resumeAt48 = -1
			}
			chunk := make([]byte, frameBytes)
			copy(chunk, s.fifo[:frameBytes])
			s.fifo = s.fifo[frameBytes:]
			if err := s.writePCMFrameTracked(PCMFrame{
				Data:      chunk,
				PTS48:     pts,
				NbSamples: 960,
			}); err != nil {
				return err
			}
			s.outPTS48Next += 960
		}
	}
	return nil
}

func (s *PCMStreamer) writePCMFrameTracked(f PCMFrame) error {
	if err := writePCMFrame(s.pw, f); err != nil {
		return err
	}

	// Track what we produced
	s.producedMu.Lock()
	s.lastProducedPTS48 = f.PTS48 + int64(f.NbSamples) // End of this frame
	s.producedMu.Unlock()

	return nil
}

func writePCMFrame(w io.Writer, f PCMFrame) error {
	var hdr [16]byte
	binary.BigEndian.PutUint64(hdr[0:8], uint64(f.PTS48))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(f.NbSamples))
	binary.BigEndian.PutUint32(hdr[12:16], uint32(len(f.Data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(f.Data)
	return err
}
