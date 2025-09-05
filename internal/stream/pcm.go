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

	// jitter buffer for smoothed output (PCM 48k S16 stereo, framed in 960-sample chunks)
	jbMu       sync.Mutex
	jb         [][]byte // each entry is exactly 3840 bytes (960*2*2)
	jbMax      int      // capacity in frames (e.g., 20 = 400 ms)
	jbPrefill  int      // desired prefill before draining (e.g., 5 = 100 ms)
	resumedCFD bool     // whether next frames need crossfade
	cfTail     []byte   // last 960 samples from before retry (3840 bytes)

	resumeAt48 int64 // if >=0, trim decoded audio until this sample index (48k)
	cfWindow   int   // samples per channel for crossfade, default 960 (20ms)

	// drain PTS (48k) separate from production cursor to stamp headers
	drainPTS48 int64
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

	//hdrs := map[string]string{
	//	"User-Agent":      utils.RandomUserAgent(),
	//	"Referer":         "https://www.youtube.com/",
	//	"Accept":          "*/*",
	//	"Accept-Language": "en-US,en;q=0.9",
	//	"Origin":          "https://www.youtube.com",
	//	"Connection":      "keep-alive",
	//}

	//headersBlob := utils.BuildFFmpegHeaders(hdrs)
	//if headersBlob != "" {
	//	_ = dict.Set("headers", headersBlob, 0)
	//}

	//_ = dict.Set("user_agent", hdrs["User-Agent"], 0)
	//_ = dict.Set("referer", hdrs["Referer"], 0)
	//_ = dict.Set("reconnect", "1", 0)
	//_ = dict.Set("reconnect_streamed", "1", 0)
	//_ = dict.Set("reconnect_delay_max", "5", 0)
	//_ = dict.Set("rw_timeout", "15000000", 0)
	//_ = dict.Set("http_persistent", "1", 0)
	//_ = dict.Set("http_multiple", "0", 0)

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
		jb:            make([][]byte, 0, 24),
		jbMax:         20, // ~400 ms at 20ms/frame
		jbPrefill:     5,  // ~100 ms prefill
		resumeAt48:    -1,
		cfWindow:      960, // 20 ms
	}

	go ps.run(ctx2, seek, to)
	return ps, nil
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

// pushFrameToJB enqueues a 3840-byte interleaved S16LE frame into jitter buffer.
func (s *PCMStreamer) pushFrameToJB(frame []byte) {
	if len(frame) != 960*2*2 {
		return
	}
	s.jbMu.Lock()
	if len(s.jb) < s.jbMax {
		// Copy to avoid aliasing
		cp := make([]byte, len(frame))
		copy(cp, frame)
		s.jb = append(s.jb, cp)
	} else {
		// Drop oldest to keep recent continuity
		copy(s.jb[0], s.jb[1][0:0])
		s.jb = s.jb[1:]
		cp := make([]byte, len(frame))
		copy(cp, frame)
		s.jb = append(s.jb, cp)
	}
	s.jbMu.Unlock()
}

// drainFromJB blocks until at least one frame available and the buffer prefill
// is satisfied (unless already started), then returns next frame.
func (s *PCMStreamer) drainFromJB(ctx context.Context) ([]byte, error) {
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			s.jbMu.Lock()
			n := len(s.jb)
			if n > 0 {
				// If not yet satisfied prefill, wait until >= jbPrefill
				if n >= s.jbPrefill {
					out := s.jb[0]
					s.jb = s.jb[1:]
					s.jbMu.Unlock()
					return out, nil
				}
			}
			s.jbMu.Unlock()
		}
	}
}

func (s *PCMStreamer) run(ctx context.Context, seek, to *int) {
	defer func() {
		s.writerClosed = true
		_ = s.pw.Close()
	}()

	// Start JB drainer that writes framed PCM to pipe
	go func(ctx context.Context) {
		for {
			frame, err := s.drainFromJB(ctx)
			if err != nil {
				return
			}
			pts := s.drainPTS48
			if err := writePCMFrame(s.pw, PCMFrame{
				Data:      frame,
				PTS48:     pts,
				NbSamples: 960,
			}); err != nil {
				return
			}
			s.drainPTS48 += 960
		}
	}(ctx)

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
	// Keep HLS options if needed
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
	// Find same audio stream index (best audio)
	st, codec, err := fc.FindBestStream(astiav.MediaTypeAudio, -1, -1)
	if err != nil || st == nil || codec == nil {
		fc.CloseInput()
		fc.Free()
		if err != nil {
			return fmt.Errorf("find best audio stream (reopen): %w", err)
		}
		return errors.New("no audio stream found (reopen)")
	}
	// Recreate decoder
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
	// Swap contexts
	s.fc = fc
	s.audioStream = st
	s.decCtx = decCtx
	s.timeBase = st.TimeBase()
	// Seek near last output PTS (48k clock)
	if s.outPTS48Next > 0 {
		ts := astiav.RescaleQ(s.outPTS48Next, astiav.NewRational(1, 48000), s.timeBase)
		_ = s.fc.SeekFrame(s.audioStream.Index(), ts, astiav.NewSeekFlags())
		_ = s.fc.Flush()
	}
	// Clear FIFO to re-align to next 960 boundary
	s.fifo = s.fifo[:0]
	// Reset SWR init so it re-initializes on next frame
	s.initedSWR = false
	s.inRate = 0
	s.inLayout = astiav.ChannelLayout{}

	// Arrange sample-accurate trim and crossfade
	s.resumeAt48 = s.outPTS48Next
	s.jbMu.Lock()
	// capture tail for crossfade (last 20ms if available)
	s.cfTail = nil
	if n := len(s.jb); n > 0 {
		last := s.jb[n-1]
		if len(last) == 960*2*2 {
			s.cfTail = make([]byte, len(last))
			copy(s.cfTail, last)
		}
	}
	// Clear JB so we don’t replay old frames
	s.jb = s.jb[:0]
	s.jbMu.Unlock()
	s.resumedCFD = true
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
		s.drainPTS48 = s.firstPTS48
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
	const frameBytes = 960 * 2 * 2 // 3840 bytes
	s.fifo = append(s.fifo, interleaved...)
	for len(s.fifo) >= frameBytes {
		pts := s.outPTS48Next
		// Trim logic on resume: ensure we start exactly at resumeAt48
		if s.resumeAt48 >= 0 {
			// If this frame ends before resume target, drop it
			if pts+960 <= s.resumeAt48 {
				s.outPTS48Next += 960
				s.fifo = s.fifo[frameBytes:]
				continue
			}
			// If target lies inside this frame, slice from exact offset
			if pts < s.resumeAt48 && s.resumeAt48 < pts+960 {
				// offset in samples/ch
				offSamp := int(s.resumeAt48 - pts) // 0..959
				// slice bytes from interleaved stereo 16-bit: 2ch * 2B * offSamp
				byteOff := offSamp * 4
				// Take tail [offSamp..960), then we will pad next frame logic to align to 960 boundary:
				partial := make([]byte, frameBytes-byteOff)
				copy(partial, s.fifo[byteOff:frameBytes])
				// We still emit a full 960-sample frame to downstream; rebuild a full frame by
				// concatenating with upcoming samples (simple approach: emit shorter first frame this
				// time and align on next iterations). To keep downstream contract (always 960),
				// push the partial into fifo front to combine with next decoded PCM:
				s.fifo = s.fifo[frameBytes:]
				// Prepend partial back to fifo head so next append will grow it to >=3840 again
				s.fifo = append(partial, s.fifo...)
				// Align outPTS to resumeAt48
				s.outPTS48Next = s.resumeAt48
				// Clear resume guard so subsequent frames flow normally
				s.resumeAt48 = -1
				continue
			}
			// pts >= resumeAt48: resume alignment complete
			s.resumeAt48 = -1
		}
		// Normal emission
		chunk := make([]byte, frameBytes)
		copy(chunk, s.fifo[:frameBytes])
		s.fifo = s.fifo[frameBytes:]
		// Apply crossfade exactly once after resume, if we have a tail
		if s.resumedCFD && s.cfTail != nil && len(s.cfTail) == frameBytes {
			s.applyCrossfadeInPlace(chunk, s.cfTail, s.cfWindow)
			s.cfTail = nil
			s.resumedCFD = false
		}
		// Enqueue to jitter buffer (producer side)
		s.pushFrameToJB(chunk)
		// Maintain PTS for framing metadata
		s.outPTS48Next += 960
		// Also emit framed header to pipe from a separate goroutine if not already
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
			if s.resumedCFD && s.cfTail != nil && len(s.cfTail) == frameBytes {
				s.applyCrossfadeInPlace(chunk, s.cfTail, s.cfWindow)
				s.cfTail = nil
				s.resumedCFD = false
			}
			s.pushFrameToJB(chunk)
			s.outPTS48Next += 960
		}
	}
	return nil
}

// applyCrossfadeInPlace performs a linear crossfade for window samples (per ch)
// between prevTail (fade-out) and curHead (fade-in). Both are 3840-byte frames.
func (s *PCMStreamer) applyCrossfadeInPlace(curHead, prevTail []byte, window int) {
	if window <= 0 || window > 960 {
		window = 960
	}
	// Operate on the first window samples of curHead and entire prevTail’s last window samples.
	// Here, both are exactly one frame (960 samples), so we just crossfade all 960 samples.
	for i := 0; i < window; i++ {
		// Two channels
		for ch := 0; ch < 2; ch++ {
			off := (i*2 + ch) * 2
			// prevTail sample
			pv := int16(uint16(prevTail[off]) | uint16(prevTail[off+1])<<8)
			// curHead sample
			cv := int16(uint16(curHead[off]) | uint16(curHead[off+1])<<8)
			// linear weights
			aNum := i
			aDen := window - 1
			// out = pv*(1-a) + cv*a
			// use 32-bit accumulator
			var out int32
			if aDen <= 0 {
				out = int32(cv)
			} else {
				out = (int32(pv)*(int32(aDen-aNum)) + int32(cv)*int32(aNum)) / int32(aDen)
			}
			if out > 32767 {
				out = 32767
			} else if out < -32768 {
				out = -32768
			}
			curHead[off] = byte(uint16(out) & 0xff)
			curHead[off+1] = byte(uint16(out) >> 8)
		}
	}
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
