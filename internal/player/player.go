package player

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
	DefaultVol      int
	LoopSong        bool
	LoopQueue       bool
	DisconnectTimer *time.Timer
	LastURL         string

	requestedSeek *int

	curPlay *playSession
}

type playSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	pcm *stream.PCMStreamer
	enc *stream.Encoder

	doneCh chan struct{}
}

func NewPlayer(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache, guildID string) *Player {
	return &Player{
		cfg:        cfg,
		repo:       repo,
		cache:      cache,
		guildID:    guildID,
		Status:     StatusIdle,
		DefaultVol: DefaultVolume,
	}
}

func (p *Player) Connect(s *discordgo.Session, guildID, channelID string) error {
	p.mu.Lock()
	// already on the same channel
	if p.Conn != nil && p.Conn.ChannelID == channelID {
		p.mu.Unlock()
		return nil
	}
	// disconnect old connection (no network work under lock)
	old := p.Conn
	p.Conn = nil
	p.mu.Unlock()

	if old != nil {
		_ = old.Speaking(false)
		old.Disconnect()
	}

	vc, err := s.ChannelVoiceJoin(guildID, channelID, false, true)
	if err != nil {
		return err
	}

	// Load settings for default volume outside lock
	ctx := context.Background()
	defVol := DefaultVolume
	if sset, err := p.repo.GetSettings(ctx, p.guildID); err == nil && sset != nil {
		defVol = sset.DefaultVolume
	}

	p.mu.Lock()
	p.Conn = vc
	p.DefaultVol = defVol
	// any pending idle disconnect for previous state should be canceled
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return nil
}

func (p *Player) Disconnect() {
	p.mu.Lock()
	// stop any playback
	p.stopPlayLocked()

	p.Status = StatusIdle
	p.NowPlaying = nil
	p.PositionSec = 0

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}

	vc := p.Conn
	p.Conn = nil
	p.mu.Unlock()

	if vc != nil {
		_ = vc.Speaking(false)
		vc.Disconnect()
	}
}

func (p *Player) Add(song SongMetadata, immediate bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if song.Playlist != nil || !immediate || len(p.SongQueue) == 0 {
		p.SongQueue = append(p.SongQueue, song)
		return
	}

	insertAt := p.Qpos + 1
	if insertAt < 0 {
		insertAt = 0
	}
	if insertAt > len(p.SongQueue) {
		insertAt = len(p.SongQueue)
	}

	// insert while preserving order
	p.SongQueue = append(p.SongQueue, SongMetadata{})      // grow by one
	copy(p.SongQueue[insertAt+1:], p.SongQueue[insertAt:]) // shift right
	p.SongQueue[insertAt] = song
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

func (p *Player) GetQueuePage(page, pageSize int) ([]SongMetadata, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Visible queue excludes the current song: p.SongQueue[p.Qpos+1:]
	if p.Qpos+1 >= len(p.SongQueue) {
		return []SongMetadata{}, 0
	}

	visible := p.SongQueue[p.Qpos+1:]
	total := len(visible)

	start := (page - 1) * pageSize
	if start >= total {
		return []SongMetadata{}, total
	}

	end := start + pageSize
	if end > total {
		end = total
	}

	out := make([]SongMetadata, end-start)
	copy(out, visible[start:end])
	return out, total
}

func (p *Player) QueueSize() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.SongQueue)-p.Qpos-1 < 0 {
		return 0
	}
	return len(p.SongQueue) - p.Qpos - 1
}

func (p *Player) MaybeAutoplayAfterAdd(ctx context.Context, s *discordgo.Session) {
	p.mu.Lock()
	shouldPlay := p.Status != StatusPlaying && p.currentLocked() != nil
	p.cancelIdleDisconnectLocked()
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
	if nb != 960 || n != 960*2*2 {
		return framedPCM{}, fmt.Errorf("bad frame sizes nb=%d n=%d", nb, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return framedPCM{}, err
	}
	return framedPCM{pts48: pts48, nbSamples: nb, data: buf}, nil
}

func (p *Player) Play(ctx context.Context, s *discordgo.Session) error {
	// Read minimal state and stop any current play under lock
	p.mu.Lock()
	vc := p.Conn
	cur := p.currentLocked()
	if vc == nil {
		p.mu.Unlock()
		return errors.New("not connected")
	}
	if cur == nil {
		p.mu.Unlock()
		return errors.New("queue empty")
	}

	// stop any existing play session
	p.stopPlayLocked()

	// resolve seek/to
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

	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	// Resolve input URL without holding the lock
	inputURL := ""
	if cur.Source == SourceHLS {
		inputURL = cur.URL
	} else {
		inputURL = cur.URL
		if !strings.HasPrefix(inputURL, "http") {
			ytURL := "https://www.youtube.com/watch?v=" + cur.VideoID
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
	}

	// Create playback-scoped context and resources
	playCtx, playCancel := context.WithCancel(ctx)
	pcm, err := stream.StartPCMStream(playCtx, inputURL, seek, to)
	if err != nil {
		playCancel()
		return err
	}
	enc, err := stream.NewEncoder()
	if err != nil {
		pcm.Close()
		playCancel()
		return err
	}

	sess := &playSession{
		ctx:    playCtx,
		cancel: playCancel,
		pcm:    pcm,
		enc:    enc,
		doneCh: make(chan struct{}),
	}

	// Commit the session and state if still valid
	p.mu.Lock()
	if p.Conn == nil || p.Conn != vc || p.currentLocked() != cur {
		// state changed while preparing; abort
		p.mu.Unlock()
		sess.cancel()
		enc.Close()
		pcm.Close()
		return errors.New("play aborted due to state change")
	}
	p.curPlay = sess
	p.Status = StatusPlaying
	p.NowPlaying = cur
	p.LastURL = cur.URL
	p.PositionSec = pos
	p.mu.Unlock()

	// Start sender loop
	go p.sendLoop(vc, cur, pos, sess)

	return nil
}

func (p *Player) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Status != StatusPlaying {
		return errors.New("not playing")
	}
	p.Status = StatusPaused
	p.stopPlayLocked()
	if p.Conn != nil {
		_ = p.Conn.Speaking(false)
	}
	return nil
}

func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopPlayLocked()

	p.Status = StatusIdle
	p.SongQueue = nil
	p.Qpos = 0
	p.NowPlaying = nil
	p.PositionSec = 0

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}

	if p.Conn != nil {
		_ = p.Conn.Speaking(false)
	}
}

func (p *Player) Forward(ctx context.Context, s *discordgo.Session, n int) error {
	p.mu.Lock()
	p.stopPlayLocked()

	if p.Qpos+n-1 >= len(p.SongQueue) {
		// queue empty; stay idle and schedule disconnect
		p.Status = StatusIdle
		p.PositionSec = 0
		p.mu.Unlock()

		p.scheduleIdleDisconnect()
		return nil
	}

	p.Qpos += n
	p.PositionSec = 0
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s)
}

func (p *Player) Back(ctx context.Context, s *discordgo.Session) error {
	p.mu.Lock()
	p.stopPlayLocked()

	if p.Qpos-1 < 0 {
		p.mu.Unlock()
		return errors.New("no previous")
	}
	p.Qpos--
	p.PositionSec = 0
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s)
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
	p.stopPlayLocked()
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s)
}

func (p *Player) GetPosition() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.PositionSec
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
	// Visible queue is p.SongQueue[p.Qpos+1:]
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
	if p.Status == StatusPlaying {
		p.mu.Unlock()
		return errors.New("already playing")
	}
	cur := p.currentLocked()
	if cur == nil {
		p.mu.Unlock()
		return errors.New("nothing to play")
	}
	pos := p.PositionSec
	p.requestedSeek = &pos
	p.stopPlayLocked()
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s)
}

// stopPlayLocked stops the current play session. Caller must hold p.mu.
// It will temporarily release the lock while waiting for the goroutine to end.
func (p *Player) stopPlayLocked() {
	if p.curPlay == nil {
		return
	}
	sess := p.curPlay
	p.curPlay = nil

	// cancel first so the loop stops
	sess.cancel()

	// wait for sender goroutine to exit without holding the lock
	done := sess.doneCh
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	p.mu.Lock()
}

func (p *Player) scheduleIdleDisconnect() {
	// Load setting outside lock (can block)
	set, _ := p.repo.GetSettings(context.Background(), p.guildID)
	if set == nil || set.SecondsWaitAfterEmpty == 0 {
		return
	}
	wait := time.Duration(set.SecondsWaitAfterEmpty) * time.Second

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
	}
	p.DisconnectTimer = time.AfterFunc(wait, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.Status == StatusIdle && p.curPlay == nil && p.Conn != nil {
			_ = p.Conn.Speaking(false)
			p.Conn.Disconnect()
			p.Conn = nil
		}
	})
}

func (p *Player) cancelIdleDisconnectLocked() {
	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}
}

func (p *Player) sendLoop(
	vc *discordgo.VoiceConnection,
	cur *SongMetadata,
	startPos int,
	sess *playSession,
) {
	defer func() {
		sess.enc.Close()
		sess.pcm.Close()
		sess.cancel()
		close(sess.doneCh)
	}()

	// Wait voice connection ready
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (vc == nil || !vc.Ready) {
		select {
		case <-sess.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if vc == nil || !vc.Ready {
		return
	}

	_ = vc.Speaking(true)
	defer vc.Speaking(false)

	r := bufio.NewReaderSize(sess.pcm.Stdout(), 128*1024)
	framePCM := make([]byte, sess.enc.FrameBytes())

	var wall0 time.Time
	var media0 int64
	started := false

	updatePosition := func(pts48 int64) {
		sec := int(pts48 / 48000)
		p.mu.Lock()
		// only update if still current session
		if p.curPlay == sess {
			p.PositionSec = sec
		}
		p.mu.Unlock()
	}

	for {
		f, err := readPCMFrame(r)
		if err != nil {
			break
		}

		if !started {
			started = true
			wall0 = time.Now()
			media0 = f.pts48
			updatePosition(f.pts48)

			// ensure we record that we've started, using the existing fields
			p.mu.Lock()
			if p.curPlay == sess {
				// already set to playing in Play()
				_ = startPos // consumed
			}
			p.mu.Unlock()
		}

		copy(framePCM, f.data)

		var outPkt []byte
		if err := sess.enc.EncodeFrame(framePCM, func(pkt []byte) error {
			outPkt = append(outPkt[:0], pkt...)
			return nil
		}); err != nil {
			break
		}
		if len(outPkt) == 0 {
			continue
		}

		offset := time.Duration((f.pts48 - media0) * int64(time.Second) / 48000)
		target := wall0.Add(offset)
		if d := time.Until(target); d > 0 {
			select {
			case <-sess.ctx.Done():
				return
			case <-time.After(d):
			}
		}

		select {
		case <-sess.ctx.Done():
			return
		case vc.OpusSend <- outPkt:
			updatePosition(f.pts48)
		case <-time.After(200 * time.Millisecond):
			// drop if blocked
		}
	}

	// Finished or errored. Decide next action.
	p.mu.Lock()
	// If superseded by another session, just exit.
	if p.curPlay != sess {
		p.mu.Unlock()
		return
	}

	// Clear current session state
	p.curPlay = nil
	p.Status = StatusIdle
	p.PositionSec = 0

	advance := true
	if p.LoopSong {
		seek0 := 0
		p.requestedSeek = &seek0
		advance = false
	} else if p.LoopQueue && len(p.SongQueue) > 0 && p.Qpos >= 0 && p.Qpos < len(p.SongQueue) {
		item := p.SongQueue[p.Qpos]
		p.SongQueue = append(p.SongQueue[:p.Qpos], p.SongQueue[p.Qpos+1:]...)
		p.SongQueue = append(p.SongQueue, item)
	} else {
		p.Qpos++
	}

	hasNext := p.Qpos >= 0 && p.Qpos < len(p.SongQueue)
	p.mu.Unlock()

	if advance && !hasNext {
		p.scheduleIdleDisconnect()
		return
	}

	// Start next or replay
	_ = p.Play(context.Background(), nil)
}
