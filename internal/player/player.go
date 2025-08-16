package player

import (
	"context"
	"errors"
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

	// Resume
	if p.Status == StatusPaused && p.NowPlaying != nil && cur.URL == p.NowPlaying.URL {
		p.Status = StatusPlaying
		p.startTracking(0)
		return nil
	}

	p.stopTracking()

	// Prepare stream
	var seek *int
	var to *int
	pos := 0
	if cur.Offset > 0 {
		val := cur.Offset
		seek = &val
		t := cur.Length + cur.Offset
		to = &t
		pos = cur.Offset
	}

	// Resolve media input URL
	inputURL := ""
	var volumeAdj *string
	volumeAdj = nil // implement loudness if desired by probing formats; omitted here

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
		inputURL = stream.YtdlpAudioURL(info)
		if inputURL == "" {
			return errors.New("no audio URL")
		}
	}

	// Start ffmpeg -> opus
	streamer, err := stream.StartOpusStream(ctx, inputURL, seek, to, volumeAdj)
	if err != nil {
		return err
	}

	p.Status = StatusPlaying
	p.NowPlaying = cur
	p.LastURL = cur.URL
	p.PositionSec = pos
	p.startTracking(pos)

	// Send opus frames in a goroutine
	go func(vc *discordgo.VoiceConnection, st *stream.OpusStreamer, song *SongMetadata) {
		err := stream.SendOpus(vc, st)
		st.Close()

		// Do NOT hold p.mu while calling methods that acquire p.mu themselves.
		p.mu.Lock()
		loopingSong := p.LoopSong && p.Status == StatusPlaying
		loopingQueue := p.LoopQueue && p.Status == StatusPlaying && song != nil
		p.mu.Unlock()

		if err != nil {
			// Move to next track, or schedule idle
			slog.Error("send opus stream", "song", song.Title, "err", err)
			_ = p.Forward(ctx, s, 1)
			return
		}

		if loopingSong {
			// Seek to 0
			_ = p.Seek(ctx, s, 0)
			return
		}

		if loopingQueue {
			// Re-add current song to end of queue
			p.mu.Lock()
			if song != nil {
				p.SongQueue = append(p.SongQueue, *song)
			}
			p.mu.Unlock()
		}

		_ = p.Forward(ctx, s, 1)
	}(p.Conn, streamer, cur)

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
	return nil
}

func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopTracking()
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

	if p.Qpos+n-1 >= len(p.SongQueue) {
		// queue empty, plan disconnect if configured
		p.Status = StatusIdle
		p.stopTracking()
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
	p.stopTracking()
	go func() { _ = p.Play(ctx, s) }()
	return nil
}

func (p *Player) Back(ctx context.Context, s *discordgo.Session) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Qpos-1 < 0 {
		return errors.New("no previous")
	}
	p.Qpos--
	p.PositionSec = 0
	p.stopTracking()
	go func() { _ = p.Play(ctx, s) }()
	return nil
}

func (p *Player) Seek(ctx context.Context, s *discordgo.Session, posSec int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.currentLocked()
	if cur == nil {
		return errors.New("nothing playing")
	}
	if cur.IsLive {
		return errors.New("can't seek in live")
	}
	if posSec > cur.Length {
		return errors.New("seek past end")
	}
	p.Status = StatusPaused
	p.stopTracking()
	// We just restart stream with seek
	p.mu.Unlock()
	err := p.Play(ctx, s) // Play() pulls offset from cur.Offset, but we want absolute
	p.mu.Lock()
	p.PositionSec = posSec
	return err
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
	if p.currentLocked() == nil {
		return errors.New("nothing to play")
	}
	go func() { _ = p.Play(ctx, s) }()
	return nil
}

func (p *Player) PauseCmd() error {
	return p.Pause()
}

// GetQueuePage returns a slice of the queue (excluding current) with pagination.
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
