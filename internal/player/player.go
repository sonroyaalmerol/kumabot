package player

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
	"github.com/sonroyaalmerol/kumabot/internal/stream"
)

const DefaultVolume = 100

type Player struct {
	cfg     *config.Config
	repo    *repository.Repo
	cache   *cache.FileCache
	guildID string

	mu              sync.Mutex
	Conn            *discordgo.VoiceConnection
	Status          PlayerStatus
	SongQueue       []SongMetadata
	Qpos            int
	NowPlaying      *SongMetadata
	PositionSec     int
	Timer           *time.Ticker
	StopPos         chan struct{}
	DefaultVol      int
	LoopSong        bool
	LoopQueue       bool
	DisconnectTimer *time.Timer
	LastURL         string

	playCtx       context.Context
	playCancel    context.CancelFunc
	requestedSeek *int // absolute seek (seconds) for next Play() start
}

func NewPlayer(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache, guildID string) *Player {
	return &Player{
		cfg:        cfg,
		repo:       repo,
		cache:      cache,
		guildID:    guildID,
		Status:     StatusIdle,
		DefaultVol: DefaultVolume,
		StopPos:    make(chan struct{}, 1),
	}
}

func (p *Player) Connect(s *discordgo.Session, guildID, channelID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Conn != nil && p.Conn.ChannelID == channelID {
		return nil
	}
	if p.Conn != nil {
		p.Conn.Disconnect()
		p.Conn = nil
	}
	vc, err := s.ChannelVoiceJoin(guildID, channelID, false, true)
	if err != nil {
		return err
	}
	p.Conn = vc
	// Load settings for default volume
	ctx := context.Background()
	if s, err := p.repo.GetSettings(ctx, p.guildID); err == nil {
		p.DefaultVol = s.DefaultVolume
	}
	return nil
}

func (p *Player) Disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Conn != nil {
		p.Conn.Speaking(false)
		p.Conn.Disconnect()
		p.Conn = nil
	}
	p.Status = StatusIdle
	p.NowPlaying = nil
	p.stopTracking()
}

func (p *Player) Add(song SongMetadata, immediate bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if song.Playlist != nil || !immediate || len(p.SongQueue) == 0 {
		p.SongQueue = append(p.SongQueue, song)
	} else {
		insertAt := p.Qpos + 1
		if insertAt < 0 {
			insertAt = 0
		}
		if insertAt > len(p.SongQueue) {
			insertAt = len(p.SongQueue)
		}
		p.SongQueue = append(p.SongQueue[:insertAt], append([]SongMetadata{song}, p.SongQueue[insertAt:]...)...)
	}
}

func (p *Player) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	var newq []SongMetadata
	cur := p.currentLocked()
	if cur != nil {
		newq = append(newq, *cur)
	}
	p.SongQueue = newq
	p.Qpos = 0
}

func (p *Player) currentLocked() *SongMetadata {
	if p.Qpos >= 0 && p.Qpos < len(p.SongQueue) {
		return &p.SongQueue[p.Qpos]
	}
	return nil
}

func (p *Player) GetCurrent() *SongMetadata {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentLocked()
}

func (p *Player) QueueSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.SongQueue)-p.Qpos-1 < 0 {
		return 0
	}
	return len(p.SongQueue) - p.Qpos - 1
}

func (p *Player) MaybeAutoplayAfterAdd(
	ctx context.Context,
	s *discordgo.Session,
) {
	p.mu.Lock()
	shouldPlay := p.Status != StatusPlaying && p.currentLocked() != nil
	p.mu.Unlock()
	if shouldPlay {
		go func() { _ = p.Play(ctx, s) }()
	}
}

type framedPCM struct {
	pts48     int64
	nbSamples int32
	data      []byte // 960*2*2 bytes
}

func readPCMFrame(r *bufio.Reader) (framedPCM, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return framedPCM{}, err
	}
	pts48 := int64(binary.BigEndian.Uint64(hdr[0:8]))
	nb := int32(binary.BigEndian.Uint32(hdr[8:12]))
	n := int(binary.BigEndian.Uint32(hdr[12:16]))
	if n <= 0 || nb <= 0 {
		return framedPCM{}, io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return framedPCM{}, err
	}
	return framedPCM{pts48: pts48, nbSamples: nb, data: buf}, nil
}

func (p *Player) Play(ctx context.Context, s *discordgo.Session) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.Conn == nil {
		return errors.New("not connected")
	}
	cur := p.currentLocked()
	if cur == nil {
		return errors.New("queue empty")
	}

	// Cancel existing playback
	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}

	var seek *int
	var to *int
	pos := 0
	if p.requestedSeek != nil {
		val := *p.requestedSeek
		seek = &val
		if cur.Length > 0 {
			t := cur.Length
			to = &t
		}
		pos = val
		p.requestedSeek = nil
	} else if cur.Offset > 0 {
		val := cur.Offset
		seek = &val
		t := cur.Length + cur.Offset
		to = &t
		pos = cur.Offset
	}

	inputURL := ""
	if cur.Source == SourceHLS {
		inputURL = cur.URL
	} else {
		ytURL := cur.URL
		if !strings.HasPrefix(ytURL, "http") {
			ytURL = "https://www.youtube.com/watch?v=" + cur.URL
		}
		info, err := stream.YtdlpGetInfo(ctx, ytURL)
		if err != nil {
			return err
		}
		mu := stream.PickMediaURL(info)
		if mu.URL == "" {
			return errors.New("no usable media URL")
		}
		inputURL = mu.URL
	}

	playCtx, playCancel := context.WithCancel(ctx)
	p.playCtx = playCtx
	p.playCancel = playCancel

	pcm, err := stream.StartPCMStream(playCtx, inputURL, seek, to)
	if err != nil {
		playCancel()
		p.playCancel = nil
		return err
	}

	enc, err := stream.NewEncoder()
	if err != nil {
		pcm.Close()
		playCancel()
		p.playCancel = nil
		return err
	}

	// Opus packet ring
	const ringCap = 32
	type opusPkt struct {
		b    []byte
		n    int
		pts48 int64 // media time corresponding to start of this packet
	}
	ring := make([]opusPkt, ringCap)
	for i := range ring {
		ring[i].b = make([]byte, 4096)
	}
	var rHead, rTail, rSize int
	ringMu := sync.Mutex{}
	ringCond := sync.NewCond(&ringMu)
	eos := false

	pushPacket := func(pkt []byte, pts48 int64) error {
		ringMu.Lock()
		defer ringMu.Unlock()
		if eos {
			return nil
		}
		if rSize == ringCap {
			// drop oldest to limit latency
			rTail = (rTail + 1) % ringCap
			rSize--
		}
		dst := &ring[rHead]
		if len(pkt) > len(dst.b) {
			dst.b = make([]byte, len(pkt))
		}
		copy(dst.b, pkt)
		dst.n = len(pkt)
		dst.pts48 = pts48
		rHead = (rHead + 1) % ringCap
		rSize++
		ringCond.Signal()
		return nil
	}

	// Encode goroutine: consumes framed PCM with PTS and emits Opus with PTS
	go func() {
		defer func() {
			pcm.Close()
			enc.Close()
			ringMu.Lock()
			eos = true
			ringCond.Broadcast()
			ringMu.Unlock()
		}()

		r := bufio.NewReaderSize(pcm.Stdout(), 128*1024)
		framePCM := make([]byte, enc.FrameBytes())
		for {
			f, err := readPCMFrame(r)
			if err != nil {
				if errors.Is(err, io.EOF) {
					_ = enc.Flush(func(_ []byte) error { return nil })
				}
				return
			}
			// Safety: expect exactly one opus frame worth per PCMFrame
			if len(f.data) != enc.FrameBytes() || f.nbSamples != 960 {
				// If ever mismatched, split/accumulate here (not expected with our streamer)
				continue
			}
			copy(framePCM, f.data)
			curPTS48 := f.pts48
			err = enc.EncodeFrame(framePCM, func(pkt []byte) error {
				return pushPacket(pkt, curPTS48)
			})
			if err != nil {
				return
			}
		}
	}()

	// Update state before sending
	p.Status = StatusPlaying
	p.NowPlaying = cur
	p.LastURL = cur.URL
	p.PositionSec = pos

	// Sender: schedule by media PTS
	go func(vc *discordgo.VoiceConnection, startedAt int, cancel context.CancelFunc) {
		defer func() {
			p.mu.Lock()
			p.stopTracking()
			p.mu.Unlock()
		}()

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && (vc == nil || !vc.Ready) {
			select {
			case <-playCtx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		if vc == nil || !vc.Ready {
			return
		}

		_ = vc.Speaking(true)
		defer vc.Speaking(false)

		var wall0 time.Time
		var media0 int64
		var started bool
		var lastSentPTS48 int64

		updatePosition := func(pts48 int64) {
			sec := int(pts48 / 48000)
			p.mu.Lock()
			p.PositionSec = sec
			p.mu.Unlock()
		}

		for {
			// Pop one packet
			ringMu.Lock()
			for rSize == 0 && !eos {
				if playCtx.Err() != nil {
					ringMu.Unlock()
					return
				}
				ringCond.Wait()
			}
			if rSize == 0 && eos {
				ringMu.Unlock()
				return
			}
			pkt := ring[rTail]
			rTail = (rTail + 1) % ringCap
			rSize--
			ringMu.Unlock()

			if !started {
				started = true
				wall0 = time.Now()
				media0 = pkt.pts48
				// sync UI position to media
				updatePosition(pkt.pts48)
				p.mu.Lock()
				p.startTracking(startedAt)
				p.mu.Unlock()
			}

			// Target time based on media PTS
			// target = wall0 + (pkt.pts48 - media0)/48000
			offset := time.Duration((pkt.pts48 - media0) * int64(time.Second) / 48000)
			target := wall0.Add(offset)
			now := time.Now()
			if target.After(now) {
				time.Sleep(target.Sub(now))
			} else {
				// If we are late by more than one frame, we could consider dropping
				// to catch up. For now, we just send immediately.
			}

			select {
			case <-playCtx.Done():
				return
			case vc.OpusSend <- pkt.b[:pkt.n]:
				lastSentPTS48 = pkt.pts48
				updatePosition(lastSentPTS48)
			case <-time.After(200 * time.Millisecond):
				// Drop if blocked, do not rewind schedule
			}
		}
	}(p.Conn, pos, playCancel)

	return nil
}

func (p *Player) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Status != StatusPlaying {
		return errors.New("not playing")
	}
	p.Status = StatusPaused
	p.stopTracking()
	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}
	return nil
}

func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopTracking()
	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}
	p.Status = StatusIdle
	if p.Conn != nil {
		p.Conn.Speaking(false)
	}
	p.SongQueue = nil
	p.Qpos = 0
	p.NowPlaying = nil
}

func (p *Player) Forward(ctx context.Context, s *discordgo.Session, n int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}
	p.stopTracking()

	if p.Qpos+n-1 >= len(p.SongQueue) {
		// queue empty, plan disconnect if configured
		p.Status = StatusIdle
		set, _ := p.repo.GetSettings(ctx, p.guildID)
		if set != nil && set.SecondsWaitAfterEmpty != 0 {
			if p.DisconnectTimer != nil {
				p.DisconnectTimer.Stop()
			}
			p.DisconnectTimer = time.AfterFunc(time.Duration(set.SecondsWaitAfterEmpty)*time.Second, func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				if p.Status == StatusIdle && p.Conn != nil {
					p.Conn.Disconnect()
					p.Conn = nil
				}
			})
		}
		return nil
	}
	p.Qpos += n
	p.PositionSec = 0
	go func() { _ = p.Play(ctx, s) }()
	return nil
}

func (p *Player) Back(ctx context.Context, s *discordgo.Session) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}
	p.stopTracking()

	if p.Qpos-1 < 0 {
		return errors.New("no previous")
	}
	p.Qpos--
	p.PositionSec = 0
	go func() { _ = p.Play(ctx, s) }()
	return nil
}

func (p *Player) Seek(ctx context.Context, s *discordgo.Session, posSec int) error {
	p.mu.Lock()
	cur := p.currentLocked()
	if cur == nil {
		p.mu.Unlock()
		return errors.New("nothing playing")
	}
	if cur.IsLive {
		p.mu.Unlock()
		return errors.New("can't seek in live")
	}
	if cur.Length > 0 && posSec > cur.Length {
		p.mu.Unlock()
		return errors.New("seek past end")
	}
	// Set desired seek for next Play
	p.requestedSeek = &posSec
	p.Status = StatusPaused
	p.stopTracking()
	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}
	p.mu.Unlock()

	// Restart playback with absolute seek
	return p.Play(ctx, s)
}

func (p *Player) GetPosition() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.PositionSec
}

func (p *Player) startTracking(initial int) {
	p.PositionSec = initial
	if p.Timer != nil {
		p.Timer.Stop()
	}
	p.Timer = time.NewTicker(1 * time.Second)
	go func(t *time.Ticker, stop chan struct{}) {
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				p.PositionSec++
				p.mu.Unlock()
			case <-stop:
				return
			}
		}
	}(p.Timer, p.StopPos)
}

func (p *Player) stopTracking() {
	if p.Timer != nil {
		p.Timer.Stop()
		p.Timer = nil
	}
	select {
	case p.StopPos <- struct{}{}:
	default:
	}
}

func (p *Player) Queue() []SongMetadata {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Qpos+1 >= len(p.SongQueue) {
		return nil
	}
	cp := make([]SongMetadata, len(p.SongQueue[p.Qpos+1:]))
	copy(cp, p.SongQueue[p.Qpos+1:])
	return cp
}

func (p *Player) ToggleLoopSong() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Status == StatusIdle {
		return p.LoopSong
	}
	// Turning on loopSong should disable loopQueue
	if p.LoopQueue {
		p.LoopQueue = false
	}
	p.LoopSong = !p.LoopSong
	return p.LoopSong
}

func (p *Player) ToggleLoopQueue() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Status == StatusIdle {
		return p.LoopQueue, errors.New("no songs to loop")
	}
	if len(p.SongQueue) < 2 {
		return p.LoopQueue, errors.New("not enough songs to loop a queue")
	}
	// Turning on loopQueue should disable loopSong
	if p.LoopSong {
		p.LoopSong = false
	}
	p.LoopQueue = !p.LoopQueue
	return p.LoopQueue, nil
}

// Move moves an item in the queue (1-based positions for the queue after current song)
func (p *Player) Move(from int, to int) (SongMetadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Visible queue is p.SongQueue[p.qpos+1:]
	if from < 1 || to < 1 {
		return SongMetadata{}, errors.New("position must be at least 1")
	}
	start := p.Qpos + 1
	if start >= len(p.SongQueue) {
		return SongMetadata{}, errors.New("no items to move")
	}
	srcIdx := start + (from - 1)
	dstIdx := start + (to - 1)
	if srcIdx < start || srcIdx >= len(p.SongQueue) || dstIdx < start || dstIdx >= len(p.SongQueue) {
		return SongMetadata{}, errors.New("move index is outside the range of the queue")
	}
	item := p.SongQueue[srcIdx]
	// Remove src
	p.SongQueue = append(p.SongQueue[:srcIdx], p.SongQueue[srcIdx+1:]...)
	// Recompute dst if src < dst because of removal shift
	if dstIdx > srcIdx {
		dstIdx--
	}
	// Insert at dst
	p.SongQueue = append(p.SongQueue[:dstIdx], append([]SongMetadata{item}, p.SongQueue[dstIdx:]...)...)
	return item, nil
}

func (p *Player) RemoveFromQueue(pos int, count int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pos < 1 {
		return errors.New("position must be at least 1")
	}
	if count < 1 {
		return errors.New("range must be at least 1")
	}
	start := p.Qpos + 1
	if start >= len(p.SongQueue) {
		return errors.New("queue is empty")
	}
	begin := start + (pos - 1)
	end := begin + count
	if begin < start || begin >= len(p.SongQueue) {
		return errors.New("position out of range")
	}
	if end > len(p.SongQueue) {
		end = len(p.SongQueue)
	}
	p.SongQueue = append(p.SongQueue[:begin], p.SongQueue[end:]...)
	return nil
}

func (p *Player) Replay(ctx context.Context, s *discordgo.Session) error {
	p.mu.Lock()
	cur := p.currentLocked()
	p.mu.Unlock()
	if cur == nil {
		return errors.New("nothing is playing")
	}
	if cur.IsLive {
		return errors.New("can't replay a livestream")
	}
	return p.Seek(ctx, s, 0)
}

func (p *Player) Next(ctx context.Context, s *discordgo.Session) error {
	return p.Forward(ctx, s, 1)
}

func (p *Player) Resume(ctx context.Context, s *discordgo.Session) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Status == StatusPlaying {
		return errors.New("already playing")
	}
	cur := p.currentLocked()
	if cur == nil {
		return errors.New("nothing to play")
	}
	// Continue from current position
	pos := p.PositionSec
	p.requestedSeek = &pos

	// Ensure any lingering playback is canceled
	if p.playCancel != nil {
		p.playCancel()
		p.playCancel = nil
	}

	go func() { _ = p.Play(ctx, s) }()
	return nil
}

func (p *Player) PauseCmd() error {
	return p.Pause()
}

func (p *Player) GetQueuePage(page, pageSize int) ([]SongMetadata, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	q := p.Queue()
	total := len(q)
	start := (page - 1) * pageSize
	if start >= total {
		return []SongMetadata{}, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return q[start:end], total
}
