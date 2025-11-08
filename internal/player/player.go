package player

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	ConnChannelID   string
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

	requestedSeek   *int
	lastResolvedURL string
	lastVideoID     string
	urlResolvedAt   time.Time

	curPlay *playSession

	lastSentPTS48 int64
	lastSentMu    sync.Mutex
}

type playSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	pcm *stream.PCMStreamer
	enc *stream.Encoder
	buf *opusBuffer

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
	if p.Conn != nil && p.ConnChannelID == channelID {
		p.mu.Unlock()
		return nil
	}
	// disconnect old connection (no network work under lock)
	old := p.Conn
	p.Conn = nil
	p.ConnChannelID = ""
	p.mu.Unlock()

	if old != nil {
		_ = old.Speaking(false)
		_ = old.Disconnect(context.Background())
	}

	vc, err := s.ChannelVoiceJoin(context.Background(), guildID, channelID, false, true)
	if err != nil {
		return err
	}

	// This prevents the panic in Kill() when channels are closed
	if vc.OpusSend == nil {
		vc.OpusSend = make(chan []byte, 2)
	}
	if vc.OpusRecv == nil {
		vc.OpusRecv = make(chan *discordgo.Packet, 2)
	}

	// Load settings for default volume outside lock
	ctx := context.Background()
	defVol := DefaultVolume
	if sset, err := p.repo.GetSettings(ctx, p.guildID); err == nil && sset != nil {
		defVol = sset.DefaultVolume
	}

	p.mu.Lock()
	p.Conn = vc
	p.ConnChannelID = channelID
	p.DefaultVol = defVol
	// any pending idle disconnect for previous state should be canceled
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return nil
}

// safeDisconnect safely disconnects a voice connection with proper cleanup
func (p *Player) safeDisconnect(vc *discordgo.VoiceConnection) error {
	if vc == nil {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("Voice disconnect panic recovered",
				"panic", r,
				"guildID", p.guildID,
			)
		}
	}()

	// Ensure channels exist before disconnecting
	// This prevents panic in Kill() when it tries to close nil channels
	if vc.OpusSend == nil {
		vc.OpusSend = make(chan []byte, 2)
	}
	if vc.OpusRecv == nil {
		vc.OpusRecv = make(chan *discordgo.Packet, 2)
	}

	// Stop speaking first
	_ = vc.Speaking(false)

	// Small delay to let pending operations complete
	time.Sleep(150 * time.Millisecond)

	// Disconnect with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return vc.Disconnect(ctx)
}

func (p *Player) Disconnect() {
	p.mu.Lock()
	// stop any playback
	p.stopPlayLocked()

	p.Status = StatusIdle
	p.NowPlaying = nil
	p.PositionSec = 0

	p.lastResolvedURL = ""
	p.lastVideoID = ""
	p.urlResolvedAt = time.Time{}

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}

	vc := p.Conn
	p.Conn = nil
	p.ConnChannelID = ""
	p.mu.Unlock()

	if vc != nil {
		_ = p.safeDisconnect(vc)
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

	p.lastResolvedURL = ""
	p.lastVideoID = ""
	p.urlResolvedAt = time.Time{}
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

func (p *Player) MaybeAutoplayAfterAdd(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	p.mu.Lock()
	shouldPlay := p.Status != StatusPlaying && p.currentLocked() != nil
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	if shouldPlay {
		go func() { _ = p.Play(ctx, s, i) }()
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

func (p *Player) Play(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
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

			p.mu.Lock()
			canReuse := p.lastResolvedURL != "" &&
				p.lastVideoID == cur.VideoID &&
				time.Since(p.urlResolvedAt) < 5*time.Hour
			if canReuse {
				inputURL = p.lastResolvedURL
			}
			p.mu.Unlock()

			if !canReuse {
				info, err := stream.YtdlpGetInfo(ctx, p.cfg, ytURL)
				if err != nil {
					return err
				}
				mu := stream.PickMediaURL(info)
				if mu.URL == "" {
					return errors.New("no usable media URL")
				}
				inputURL = mu.URL

				p.mu.Lock()
				p.lastResolvedURL = inputURL
				p.lastVideoID = cur.VideoID
				p.urlResolvedAt = time.Now()
				p.mu.Unlock()
			}
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

	buffer := newOpusBuffer(100)

	sess := &playSession{
		ctx:    playCtx,
		cancel: playCancel,
		pcm:    pcm,
		enc:    enc,
		buf:    buffer,
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
	go p.sendLoop(vc, i, cur, pos, sess)

	embed := BuildPlayingEmbed(p)
	if _, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{embed},
	}); err != nil {
		slog.Warn("failed to send now-playing follow-up", "guildID", p.guildID, "err", err)
	}

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

	p.lastResolvedURL = ""
	p.lastVideoID = ""
	p.urlResolvedAt = time.Time{}

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}

	if p.Conn != nil {
		_ = p.Conn.Speaking(false)
	}
}

func (p *Player) Forward(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, n int) error {
	p.mu.Lock()
	p.stopPlayLocked()

	if p.Qpos+n-1 >= len(p.SongQueue) {
		// queue empty; stay idle and schedule disconnect
		p.Status = StatusIdle
		p.PositionSec = 0

		p.lastResolvedURL = ""
		p.lastVideoID = ""
		p.urlResolvedAt = time.Time{}

		p.mu.Unlock()

		p.scheduleIdleDisconnect()
		return nil
	}

	p.Qpos += n
	p.PositionSec = 0

	p.lastResolvedURL = ""
	p.lastVideoID = ""
	p.urlResolvedAt = time.Time{}

	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s, i)
}

func (p *Player) Back(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	p.mu.Lock()
	p.stopPlayLocked()

	if p.Qpos-1 < 0 {
		p.mu.Unlock()
		return errors.New("no previous")
	}
	p.Qpos--
	p.PositionSec = 0

	p.lastResolvedURL = ""
	p.lastVideoID = ""
	p.urlResolvedAt = time.Time{}

	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s, i)
}

func (p *Player) Seek(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, posSec int) error {
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

	return p.Play(ctx, s, i)
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

func (p *Player) Replay(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	p.mu.Lock()
	cur := p.currentLocked()
	p.mu.Unlock()
	if cur == nil {
		return errors.New("nothing is playing")
	}
	if cur.IsLive {
		return errors.New("can't replay a livestream")
	}
	return p.Seek(ctx, s, i, 0)
}

func (p *Player) Next(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	return p.Forward(ctx, s, i, 1)
}

func (p *Player) Resume(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
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

	return p.Play(ctx, s, i)
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
		vc := p.Conn
		shouldDisconnect := p.Status == StatusIdle && p.curPlay == nil && vc != nil
		if shouldDisconnect {
			p.Conn = nil
			p.ConnChannelID = ""
		}
		p.mu.Unlock()

		if shouldDisconnect {
			_ = p.safeDisconnect(vc)
		}
	})
}

func (p *Player) cancelIdleDisconnectLocked() {
	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}
}

func isVoiceReady(vc *discordgo.VoiceConnection) bool {
	if vc == nil {
		return false
	}
	// Ensure channels are initialized
	if vc.OpusSend == nil {
		vc.OpusSend = make(chan []byte, 2)
	}
	if vc.OpusRecv == nil {
		vc.OpusRecv = make(chan *discordgo.Packet, 2)
	}
	// Check if connection is ready
	return vc.OpusSend != nil
}

func (p *Player) sendLoop(
	vc *discordgo.VoiceConnection,
	i *discordgo.InteractionCreate,
	cur *SongMetadata,
	startPos int,
	sess *playSession,
) {
	defer func() {
		sess.buf.Close()
		sess.enc.Close()
		sess.pcm.Close()
		sess.cancel()
		close(sess.doneCh)
	}()

	// Wait for voice ready
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !isVoiceReady(vc) {
		select {
		case <-sess.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !isVoiceReady(vc) {
		return
	}

	_ = vc.Speaking(true)
	defer vc.Speaking(false)

	// Start producer goroutine
	go p.producePackets(sess, startPos)

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-sess.ctx.Done():
				return
			case <-ticker.C:
				count := sess.buf.BufferedCount()
				slog.Debug("buffer health",
					"buffered", count,
					"max", sess.buf.maxSize,
					"guildID", p.guildID)
			}
		}
	}()

	// Consumer: send buffered packets
	p.consumePackets(vc, sess, startPos, i)
}

func (p *Player) producePackets(sess *playSession, startPos int) {
	r := bufio.NewReaderSize(sess.pcm.Stdout(), 128*1024)
	framePCM := make([]byte, sess.enc.FrameBytes())

	var wall0 time.Time
	var media0 int64
	started := false

	reconnectCh := sess.pcm.ReconnectCh()

	for {
		select {
		case <-sess.ctx.Done():
			return
		case sig := <-reconnectCh:
			slog.Info("PCM reconnection detected",
				"guildID", p.guildID,
				"lastSentPTS", sig.LastSentPTS48)

			// Flush buffer to clear stale data
			sess.buf.Flush()

			// Reset timing anchors
			started = false
			wall0 = time.Time{}
			media0 = 0
			continue
		default:
		}

		f, err := readPCMFrame(r)
		if err != nil {
			return
		}

		if !started {
			started = true
			wall0 = time.Now()
			media0 = f.pts48
			slog.Debug("producer started",
				"guildID", p.guildID,
				"startPTS", f.pts48)
		}

		copy(framePCM, f.data)

		var outPkt []byte
		if err := sess.enc.EncodeFrame(framePCM, func(pkt []byte) error {
			outPkt = append(outPkt[:0], pkt...)
			return nil
		}); err != nil {
			return
		}
		if len(outPkt) == 0 {
			continue
		}

		offset := time.Duration((f.pts48 - media0) * int64(time.Second) / 48000)
		target := wall0.Add(offset)

		// Push to buffer with backpressure
		pushAttempts := 0
		for {
			if sess.buf.Push(outPkt, f.pts48, target) {
				break
			}
			pushAttempts++
			if pushAttempts > 100 {
				slog.Warn("buffer full timeout, dropping packet", "guildID", p.guildID)
				break
			}
			select {
			case <-sess.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

func (p *Player) consumePackets(
	vc *discordgo.VoiceConnection,
	sess *playSession,
	startPos int,
	i *discordgo.InteractionCreate,
) {
	const minBufferPackets = 20

	reconnectCh := sess.pcm.ReconnectCh()

	waitForBuffer := func() bool {
		deadline := time.Now().Add(5 * time.Second)
		for sess.buf.BufferedCount() < minBufferPackets {
			if time.Now().After(deadline) {
				slog.Warn("buffer fill timeout", "guildID", p.guildID)
				return false
			}
			select {
			case <-sess.ctx.Done():
				return false
			case <-reconnectCh:
				deadline = time.Now().Add(5 * time.Second)
				slog.Info("reconnection during buffer fill", "guildID", p.guildID)
			case <-time.After(50 * time.Millisecond):
			}
		}
		return true
	}

	if !waitForBuffer() {
		return
	}

	updatePosition := func(pts48 int64) {
		sec := int(pts48 / 48000)
		p.mu.Lock()
		if p.curPlay == sess {
			p.PositionSec = sec
		}
		p.mu.Unlock()
	}

	firstPacket := true
	droppedCount := 0
	const maxDroppedBeforeRebuffer = 5

	for {
		select {
		case sig := <-reconnectCh:
			slog.Info("reconnection in consumer, rebuffering",
				"guildID", p.guildID,
				"targetPTS", sig.LastSentPTS48)
			if !waitForBuffer() {
				return
			}
			droppedCount = 0
			continue
		default:
		}

		pkt, ok := sess.buf.Pop(sess.ctx)
		if !ok {
			break
		}

		if firstPacket {
			firstPacket = false
			updatePosition(pkt.pts48)
			slog.Info("playback started", "guildID", p.guildID, "startPTS", pkt.pts48)
		}

		if d := time.Until(pkt.targetTS); d > 0 {
			select {
			case <-sess.ctx.Done():
				return
			case <-time.After(d):
			}
		}

		select {
		case <-sess.ctx.Done():
			return
		case vc.OpusSend <- pkt.data:
			// Track last successfully sent PTS
			p.lastSentMu.Lock()
			p.lastSentPTS48 = pkt.pts48
			p.lastSentMu.Unlock()

			// Pass to PCMStreamer for reconnection reference
			sess.pcm.SetLastDeliveredPTS(pkt.pts48)

			updatePosition(pkt.pts48)
			droppedCount = 0
		case <-time.After(200 * time.Millisecond):
			droppedCount++
			slog.Debug("dropped packet",
				"guildID", p.guildID,
				"consecutive", droppedCount,
				"pts", pkt.pts48)

			if droppedCount >= maxDroppedBeforeRebuffer {
				slog.Warn("too many drops, rebuffering", "guildID", p.guildID)
				sess.buf.Flush()
				if !waitForBuffer() {
					return
				}
				droppedCount = 0
			}
		}

		buffered := sess.buf.BufferedCount()
		if buffered < 5 && buffered > 0 {
			slog.Debug("buffer running low", "count", buffered, "guildID", p.guildID)
		}
	}

	p.handlePlaybackEnd(sess, i)
}

func (p *Player) handlePlaybackEnd(sess *playSession, i *discordgo.InteractionCreate) {
	p.mu.Lock()
	if p.curPlay != sess {
		p.mu.Unlock()
		return
	}

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

	_ = p.Play(context.Background(), nil, i)
}
