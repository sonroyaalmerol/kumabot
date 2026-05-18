package player

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
	"github.com/sonroyaalmerol/kumabot/internal/stream"
	"github.com/sonroyaalmerol/kumabot/internal/utils"
)

const DefaultVolume = 100

// pcmStreamer abstracts the PCM audio source so playSession can be tested
// without a real FFmpeg stream.
type pcmStreamer interface {
	Stdout() io.Reader
	ReconnectCh() <-chan stream.ReconnectSignal
	Close()
}

type Player struct {
	urlResolvedAt    time.Time
	opCtx            context.Context
	curPlay          *playSession
	cfg              *config.Config
	onRemove         func()
	cache            *cache.FileCache
	opCancel         context.CancelFunc
	requestedSeek    *int
	Conn             *discordgo.VoiceConnection
	Session          *discordgo.Session
	NowPlaying       *SongMetadata
	DisconnectTimer  *time.Timer
	radioSearchDone  chan struct{}
	repo             *repository.Repo
	lastEmbedMessage *discordgo.Message
	LastURL          string
	guildID          string
	lastVideoID      string
	lastResolvedURL  string
	TextChannelID    string
	ConnChannelID    string
	SongQueue        []SongMetadata
	RadioHistory     []radioHistoryEntry
	Qpos             int
	DefaultVol       int
	RadioQueuedIndex int
	mu               sync.Mutex
	positionSec      atomic.Int32
	radioMode        atomic.Bool
	shuffleMode      atomic.Bool
	loopQueue        atomic.Bool
	loopSong         atomic.Bool
	searchQueue      atomic.Int32
	volume           atomic.Int32
	status           atomic.Int32
}

type playSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	pcm pcmStreamer
	enc *stream.Encoder
	buf *opusBuffer

	doneCh     chan struct{}
	producerWg sync.WaitGroup
}

func NewPlayer(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache, guildID string) *Player {
	p := &Player{
		cfg:              cfg,
		repo:             repo,
		cache:            cache,
		guildID:          guildID,
		DefaultVol:       DefaultVolume,
		RadioQueuedIndex: -1,
		RadioHistory:     make([]radioHistoryEntry, 0),
	}
	p.status.Store(int32(StatusIdle))
	p.volume.Store(int32(DefaultVolume))
	return p
}

func (p *Player) StatusPub() PlayerStatus { return PlayerStatus(p.status.Load()) }
func (p *Player) GetPosition() int        { return int(p.positionSec.Load()) }
func (p *Player) GetVolume() int          { return int(p.volume.Load()) }
func (p *Player) LoopSongPub() bool       { return p.loopSong.Load() }
func (p *Player) LoopQueuePub() bool      { return p.loopQueue.Load() }
func (p *Player) ShuffleModePub() bool    { return p.shuffleMode.Load() }
func (p *Player) IsRadioMode() bool       { return p.radioMode.Load() }

func (p *Player) IsSearching() bool { return p.searchQueue.Load() > 0 }
func (p *Player) SetSearching(s bool) {
	if s {
		p.searchQueue.Add(1)
	} else {
		p.searchQueue.Add(-1)
	}
}

// ConnPub returns whether the player has an active voice connection.
func (p *Player) ConnPub() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Conn != nil
}

// newOpCtx cancels any previous in-flight operation and creates a fresh context.
func (p *Player) newOpCtx(parent context.Context) context.Context {
	p.mu.Lock()
	if p.opCancel != nil {
		p.opCancel()
	}
	p.opCtx, p.opCancel = context.WithCancel(parent)
	ctx := p.opCtx
	p.mu.Unlock()
	return ctx
}

// cancelOps cancels any in-flight operation context. Caller must hold p.mu.
func (p *Player) cancelOpsLocked() {
	if p.opCancel != nil {
		p.opCancel()
		p.opCancel = nil
		p.opCtx = nil
	}
}

func (p *Player) setIdleState() {
	p.mu.Lock()
	p.status.Store(int32(StatusIdle))
	p.NowPlaying = nil
	p.positionSec.Store(0)
	queueEmpty := len(p.SongQueue) == 0 || p.Qpos >= len(p.SongQueue)
	channelID := p.ConnChannelID
	p.mu.Unlock()

	p.clearVoiceChannelStatusWithChannel(channelID)

	if queueEmpty {
		slog.Info("player idle with empty queue - scheduling disconnect",
			"guildID", p.guildID)
		p.scheduleIdleDisconnect()
	}
}

func (p *Player) setIdleStateLocked() bool {
	p.status.Store(int32(StatusIdle))
	p.NowPlaying = nil
	p.positionSec.Store(0)
	p.RadioQueuedIndex = -1
	p.radioSearchDone = nil
	return len(p.SongQueue) == 0 || p.Qpos >= len(p.SongQueue)
}

func (p *Player) invalidateURLCacheLocked() {
	p.lastResolvedURL = ""
	p.lastVideoID = ""
	p.urlResolvedAt = time.Time{}
}

func (p *Player) Connect(ctx context.Context, s *discordgo.Session, guildID, channelID, textChannelID string) error {
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
		_ = old.Disconnect(ctx)
	}

	vc, err := s.ChannelVoiceJoin(ctx, guildID, channelID, false, true)
	if err != nil {
		return err
	}

	// This prevents the panic in Kill() when channels are closed
	if vc.OpusSend == nil {
		vc.OpusSend = make(chan []byte, 64)
	}
	if vc.OpusRecv == nil {
		vc.OpusRecv = make(chan *discordgo.Packet, 2)
	}

	// Load settings for default volume outside lock
	defVol := DefaultVolume
	if sset, err := p.repo.GetSettings(ctx, p.guildID); err == nil && sset != nil {
		defVol = sset.DefaultVolume
	}

	p.mu.Lock()
	p.Conn = vc
	p.Session = s
	p.TextChannelID = textChannelID
	p.ConnChannelID = channelID
	p.DefaultVol = defVol
	p.volume.Store(int32(defVol))
	// any pending idle disconnect for previous state should be canceled
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return nil
}

// safeDisconnect safely disconnects a voice connection with proper cleanup
func (p *Player) safeDisconnect(ctx context.Context, vc *discordgo.VoiceConnection) error {
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
		vc.OpusSend = make(chan []byte, 64)
	}
	if vc.OpusRecv == nil {
		vc.OpusRecv = make(chan *discordgo.Packet, 2)
	}

	// Stop speaking first
	_ = vc.Speaking(false)

	// Small delay to let pending operations complete
	time.Sleep(150 * time.Millisecond)

	return vc.Disconnect(ctx)
}

func (p *Player) Disconnect(ctx context.Context) {
	p.mu.Lock()
	// stop any playback and cancel in-flight operations
	p.stopPlayLocked()
	p.cancelOpsLocked()

	p.status.Store(int32(StatusIdle))
	p.NowPlaying = nil
	p.positionSec.Store(0)
	p.RadioQueuedIndex = -1
	p.radioSearchDone = nil

	p.invalidateURLCacheLocked()

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}

	vc := p.Conn
	p.Conn = nil
	connChannelID := p.ConnChannelID
	p.ConnChannelID = ""
	p.mu.Unlock()

	if vc != nil {
		p.clearVoiceChannelStatusWithChannel(connChannelID)
		_ = p.safeDisconnect(ctx, vc)
	}

	if p.onRemove != nil {
		p.onRemove()
	}
}

func (p *Player) Add(song SongMetadata, immediate bool) *SongMetadata {
	p.mu.Lock()
	defer p.mu.Unlock()

	var replaced *SongMetadata

	// If adding a manual song (not radio-suggested), remove any radio-suggested song at the end
	if !song.IsRadioSuggestion && p.RadioQueuedIndex >= 0 {
		if p.RadioQueuedIndex < len(p.SongQueue) && p.SongQueue[p.RadioQueuedIndex].IsRadioSuggestion {
			removed := p.SongQueue[p.RadioQueuedIndex]
			replaced = &removed
			p.SongQueue = append(p.SongQueue[:p.RadioQueuedIndex], p.SongQueue[p.RadioQueuedIndex+1:]...)
		}
		p.RadioQueuedIndex = -1
	}

	if song.Playlist != nil || !immediate || len(p.SongQueue) == 0 {
		p.SongQueue = append(p.SongQueue, song)
		if song.IsRadioSuggestion {
			p.RadioQueuedIndex = len(p.SongQueue) - 1
		}
		return replaced
	}

	insertAt := min(max(p.Qpos+1, 0), len(p.SongQueue))

	// insert while preserving order
	p.SongQueue = append(p.SongQueue, SongMetadata{})      // grow by one
	copy(p.SongQueue[insertAt+1:], p.SongQueue[insertAt:]) // shift right
	p.SongQueue[insertAt] = song

	if song.IsRadioSuggestion {
		p.RadioQueuedIndex = insertAt
	}
	return replaced
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
	p.RadioQueuedIndex = -1

	p.invalidateURLCacheLocked()
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

	end := min(start+pageSize, total)

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
	shouldPlay := p.StatusPub() != StatusPlaying && p.currentLocked() != nil
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	if shouldPlay {
		go func() { _ = p.Play(ctx, s, i) }()
	}
}

type framedPCM struct {
	data      []byte
	pts48     int64
	nbSamples int32
}

func readPCMFrame(r *bufio.Reader, buf []byte) (framedPCM, error) {
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
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
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
		p.scheduleIdleDisconnect()
		return errors.New("queue empty")
	}

	// stop any existing play session
	p.stopPlayLocked()
	p.lastEmbedMessage = nil

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

	opCtx := p.newOpCtx(ctx)

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
				info, err := stream.YtdlpGetInfoWithTimeout(opCtx, p.cfg, ytURL, stream.DefaultInfoTimeout)
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
	enc, err := stream.GetPooledEncoder()
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
		stream.PutPooledEncoder(enc)
		pcm.Close()
		return errors.New("play aborted due to state change")
	}
	p.curPlay = sess
	p.status.Store(int32(StatusPlaying))
	p.NowPlaying = cur
	p.LastURL = cur.URL
	p.positionSec.Store(int32(pos))
	// If the song we're about to play was the radio suggestion, consume it
	if p.RadioQueuedIndex == p.Qpos {
		p.RadioQueuedIndex = -1
	}
	p.mu.Unlock()

	// Start sender loop
	go p.sendLoop(vc, i, cur, pos, sess)
	go p.SendNowPlayingEmbed()
	go p.startEmbedUpdater(sess.ctx)

	// Check if we should pre-queue a radio song
	p.maybeQueueRadio()

	return nil
}

func (p *Player) SendNowPlayingEmbed() {
	p.mu.Lock()
	s := p.Session
	textChanID := p.TextChannelID
	guildID := p.guildID
	existingMsg := p.lastEmbedMessage
	botID := ""
	if s != nil && s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	p.mu.Unlock()

	if s == nil || textChanID == "" {
		return
	}

	embed := BuildPlayingEmbed(p)
	components := PlayingComponents(p)

	// Try to edit the existing embed in place
	if existingMsg != nil {
		_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    textChanID,
			ID:         existingMsg.ID,
			Embeds:     &[]*discordgo.MessageEmbed{embed},
			Components: &components,
		})
		if err == nil {
			return
		}
		// Edit failed (message deleted, etc.) - fall through to send new
	}

	// Scan recent messages for old now-playing embeds to clean up.
	// This handles both: old embed not being latest, and bot restarts where
	// lastEmbedMessage is nil but stale embeds exist in channel history.
	p.cleanOldEmbeds(s, textChanID, botID)

	newMsg, err := s.ChannelMessageSendComplex(textChanID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: components,
	})
	if err != nil {
		slog.Warn("failed to send now-playing embed", "guildID", guildID, "err", err)
		return
	}

	p.mu.Lock()
	p.lastEmbedMessage = newMsg
	p.mu.Unlock()
}

// cleanOldEmbeds scans recent channel messages and deletes any now-playing embeds
// previously sent by this bot (identified by the "kumabot" footer marker).
func (p *Player) cleanOldEmbeds(s *discordgo.Session, channelID, botID string) {
	msgs, err := s.ChannelMessages(channelID, 20, "", "", "")
	if err != nil {
		return
	}
	for _, m := range msgs {
		if m.Author.ID != botID || len(m.Embeds) == 0 {
			continue
		}
		for _, e := range m.Embeds {
			if e.Footer != nil && strings.HasSuffix(e.Footer.Text, "kumabot") {
				_ = s.ChannelMessageDelete(channelID, m.ID)
				break
			}
		}
	}
}

func (p *Player) startEmbedUpdater(ctx context.Context) {
	// Update embed every 5s and voice channel status every 3s for responsive progress.
	// Embed edits are rate-limited to ~5/5s by Discord; 5s is safe.
	// Voice channel status updates are separate from message edits.
	embedTicker := time.NewTicker(5 * time.Second)
	defer embedTicker.Stop()
	statusTicker := time.NewTicker(3 * time.Second)
	defer statusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-embedTicker.C:
			playing := p.StatusPub() == StatusPlaying || p.StatusPub() == StatusPaused
			if !playing {
				return
			}
			p.SendNowPlayingEmbed()
		case <-statusTicker.C:
			playing := p.StatusPub() == StatusPlaying
			if !playing {
				return
			}
			p.UpdateVoiceChannelStatus()
		}
	}
}

// updateVoiceChannelStatus sets the voice channel status text showing current song + progress.
// This is always visible at the top of the voice channel without scrolling through chat.
func (p *Player) UpdateVoiceChannelStatus() {
	p.mu.Lock()
	s := p.Session
	channelID := p.ConnChannelID
	cur := p.NowPlaying
	pos := p.GetPosition()
	status := p.StatusPub()
	p.mu.Unlock()

	if s == nil || channelID == "" || cur == nil || status != StatusPlaying {
		return
	}

	elapsed := utils.PrettyTime(pos)
	text := elapsed
	if cur.Length > 0 && !cur.IsLive {
		text = elapsed + " / " + utils.PrettyTime(cur.Length)
	}
	text += "  ▶  " + cur.Title
	if len(text) > 500 {
		text = text[:497] + "..."
	}

	body := struct{ Status string }{Status: text}
	url := "https://discord.com/api/v10/channels/" + channelID + "/voice-status"
	if _, err := s.Request("PUT", url, body); err != nil {
		slog.Debug("failed to set voice channel status", "guildID", p.guildID, "err", err)
	}
}

func (p *Player) clearVoiceChannelStatusWithChannel(channelID string) {
	p.mu.Lock()
	s := p.Session
	p.mu.Unlock()

	if s == nil || channelID == "" {
		return
	}

	url := "https://discord.com/api/v10/channels/" + channelID + "/voice-status"
	if _, err := s.Request("DELETE", url, nil); err != nil {
		slog.Debug("failed to clear voice channel status", "guildID", p.guildID, "err", err)
	}
}

func (p *Player) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.StatusPub() != StatusPlaying {
		return errors.New("not playing")
	}
	p.status.Store(int32(StatusPaused))
	p.stopPlayLocked()
	if p.Conn != nil {
		_ = p.Conn.Speaking(false)
	}
	return nil
}

func (p *Player) Stop() {
	p.mu.Lock()
	p.stopPlayLocked()
	p.cancelOpsLocked()

	p.status.Store(int32(StatusIdle))
	p.SongQueue = nil
	p.Qpos = 0
	p.NowPlaying = nil
	p.positionSec.Store(0)
	p.RadioQueuedIndex = -1
	p.radioSearchDone = nil

	p.invalidateURLCacheLocked()
	p.lastEmbedMessage = nil

	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}

	if p.Conn != nil {
		_ = p.Conn.Speaking(false)
	}
	channelID := p.ConnChannelID
	p.mu.Unlock()

	p.clearVoiceChannelStatusWithChannel(channelID)

	slog.Info("player stopped - scheduling disconnect", "guildID", p.guildID)
	p.scheduleIdleDisconnect()
}

func (p *Player) Forward(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, n int) error {
	p.mu.Lock()
	p.stopPlayLocked()

	if p.Qpos+n >= len(p.SongQueue) {
		// Queue empty after skip — if radio is on, search for a related song
		if p.IsRadioMode() && p.NowPlaying != nil {
			p.Qpos = len(p.SongQueue)
			p.cancelIdleDisconnectLocked()
			p.invalidateURLCacheLocked()
			p.mu.Unlock()

			slog.Info("forward skipped past queue end, triggering radio",
				"guildID", p.guildID)
			go p.tryQueueRadioSong(true)
			return nil
		}

		shouldSchedule := p.setIdleStateLocked()

		p.lastResolvedURL = ""
		p.lastVideoID = ""
		p.urlResolvedAt = time.Time{}

		p.mu.Unlock()

		if shouldSchedule {
			slog.Info("forward skipped past queue end - scheduling disconnect",
				"guildID", p.guildID)
			p.scheduleIdleDisconnect()
		}
		return nil
	}

	p.Qpos += n
	p.positionSec.Store(0)

	p.invalidateURLCacheLocked()

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
	p.positionSec.Store(0)

	p.invalidateURLCacheLocked()

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
	p.status.Store(int32(StatusPaused))
	p.stopPlayLocked()
	p.cancelIdleDisconnectLocked()
	p.mu.Unlock()

	return p.Play(ctx, s, i)
}

func (p *Player) ToggleLoopSong() bool {
	if p.StatusPub() == StatusIdle {
		return p.LoopSongPub()
	}
	if p.loopQueue.Load() {
		p.loopQueue.Store(false)
	}
	p.loopSong.Store(!p.loopSong.Load())
	return p.loopSong.Load()
}

func (p *Player) ToggleLoopQueue() (bool, error) {
	if p.StatusPub() == StatusIdle {
		return p.LoopQueuePub(), errors.New("no songs to loop")
	}
	p.mu.Lock()
	songCount := len(p.SongQueue)
	p.mu.Unlock()
	if songCount < 2 {
		return p.LoopQueuePub(), errors.New("not enough songs to loop a queue")
	}
	if p.loopSong.Load() {
		p.loopSong.Store(false)
	}
	p.loopQueue.Store(!p.loopQueue.Load())
	return p.loopQueue.Load(), nil
}

func (p *Player) ToggleShuffle() bool {
	p.shuffleMode.Store(!p.shuffleMode.Load())
	return p.shuffleMode.Load()
}

func (p *Player) ShuffleOn() bool {
	p.shuffleMode.Store(true)
	return true
}

func (p *Player) SetVolume(vol int) int {
	if vol < 0 {
		vol = 0
	}
	if vol > 150 {
		vol = 150
	}
	p.volume.Store(int32(vol))
	return vol
}

// ToggleRadioMode toggles radio mode on/off. Returns the new state.
func (p *Player) ToggleRadioMode() bool {
	p.radioMode.Store(!p.radioMode.Load())
	return p.radioMode.Load()
}

// TryStartRadio attempts to start radio playback if conditions are met.
// Called when radio mode is toggled on.
func (p *Player) TryStartRadio() {
	p.mu.Lock()
	playAfter := p.Qpos >= len(p.SongQueue)
	p.mu.Unlock()
	p.startRadioSearch(true, playAfter)
}

// maybeQueueRadio checks if we should pre-queue a radio song and does so in the background.
// Called when starting playback of a song. Only queues when the current song is the last in queue.
func (p *Player) maybeQueueRadio() {
	p.startRadioSearch(true, false)
}

// startRadioSearch checks conditions and launches a background radio search.
// playNow starts playback immediately after queueing (used when queue is empty).
func (p *Player) startRadioSearch(checkQueueEnd, playNow bool) {
	p.mu.Lock()
	should := p.IsRadioMode() &&
		p.NowPlaying != nil &&
		p.Qpos >= 0 &&
		(!checkQueueEnd || p.Qpos >= len(p.SongQueue)-1) &&
		p.RadioQueuedIndex < 0 &&
		p.radioSearchDone == nil
	p.mu.Unlock()

	if !should {
		return
	}

	ch := make(chan struct{})
	p.mu.Lock()
	p.radioSearchDone = ch
	p.mu.Unlock()

	go func() {
		defer close(ch)
		p.tryQueueRadioSong(playNow)
		p.mu.Lock()
		p.radioSearchDone = nil
		p.mu.Unlock()
	}()
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
	if p.StatusPub() == StatusPlaying {
		p.mu.Unlock()
		return errors.New("already playing")
	}
	cur := p.currentLocked()
	if cur == nil {
		p.mu.Unlock()
		return errors.New("nothing to play")
	}
	pos := p.GetPosition()
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

		if p.IsSearching() {
			p.DisconnectTimer.Reset(wait)
			p.mu.Unlock()
			return
		}

		vc := p.Conn
		shouldDisconnect := p.StatusPub() == StatusIdle && p.curPlay == nil && vc != nil
		if shouldDisconnect {
			p.Conn = nil
			p.ConnChannelID = ""
		}
		onRemove := p.onRemove
		p.mu.Unlock()

		// Do blocking I/O outside the lock to prevent deadlocks.
		if shouldDisconnect && vc != nil {
			_ = p.safeDisconnect(context.Background(), vc)
			if onRemove != nil {
				onRemove()
			}
		}
	})
}

func (p *Player) cancelIdleDisconnectLocked() {
	if p.DisconnectTimer != nil {
		p.DisconnectTimer.Stop()
		p.DisconnectTimer = nil
	}
}

// applyVolumeS16LE scales S16LE PCM samples by vol/100 in-place.
func applyVolumeS16LE(pcm []byte, vol int) {
	factor := float32(vol) / 100.0
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		s = int16(float32(s) * factor)
		pcm[i] = byte(uint16(s))
		pcm[i+1] = byte(uint16(s) >> 8)
	}
}

func (p *Player) sendLoop(
	vc *discordgo.VoiceConnection,
	i *discordgo.InteractionCreate,
	cur *SongMetadata,
	startPos int,
	sess *playSession,
) {
	defer func() {
		// Unblock producer and consumer first so goroutines can exit promptly.
		sess.pcm.Close()
		sess.buf.Close()

		sess.producerWg.Wait()
		stream.PutPooledEncoder(sess.enc)
		sess.cancel()
		close(sess.doneCh)
	}()

	// Wait for voice ready
	readyTimer := time.NewTimer(100 * time.Millisecond)
	defer readyTimer.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if vc == nil {
			return
		}
		if vc.OpusSend == nil {
			vc.OpusSend = make(chan []byte, 64)
		}
		if vc.OpusRecv == nil {
			vc.OpusRecv = make(chan *discordgo.Packet, 2)
		}
		if vc.OpusSend != nil {
			break
		}
		readyTimer.Reset(100 * time.Millisecond)
		select {
		case <-sess.ctx.Done():
			if !readyTimer.Stop() {
				select {
				case <-readyTimer.C:
				default:
				}
			}
			return
		case <-readyTimer.C:
		}
	}
	if vc == nil || vc.OpusSend == nil {
		return
	}

	_ = vc.Speaking(true)
	defer func() { _ = vc.Speaking(false) }()

	// Start producer goroutine
	sess.producerWg.Add(1)
	go p.producePackets(sess, startPos)

	// Consumer: send buffered packets
	p.consumePackets(vc, sess, startPos, i)
}

func (p *Player) producePackets(sess *playSession, startPos int) {
	defer func() {
		sess.producerWg.Done()
		sess.buf.MarkEOS()
		slog.Debug("producer finished, marked EOS",
			"guildID", p.guildID,
			"buffered", sess.buf.BufferedCount())
	}()

	r := bufio.NewReaderSize(sess.pcm.Stdout(), 128*1024)
	framePCM := make([]byte, sess.enc.FrameBytes())
	readBuf := make([]byte, 0, sess.enc.FrameBytes())
	var outPkt []byte // reused across frames to avoid per-frame heap allocation

	// Reusable timer for buffer-full backoff
	pushBackoff := time.NewTimer(0)
	if !pushBackoff.Stop() {
		<-pushBackoff.C
	}
	defer pushBackoff.Stop()

	var wall0 time.Time
	var media0 int64
	started := false

	reconnectCh := sess.pcm.ReconnectCh()

	for {
		select {
		case <-sess.ctx.Done():
			return
		case sig := <-reconnectCh:
			slog.Info("PCMStreamer reconnected",
				"guildID", p.guildID,
				"resumeAtPTS", sig.LastSentPTS48)
			continue
		default:
		}

		f, err := readPCMFrame(r, readBuf)
		if err != nil {
			return
		}
		readBuf = f.data[:0]

		if !started {
			started = true
			wall0 = time.Now()
			media0 = f.pts48
			slog.Debug("producer started",
				"guildID", p.guildID,
				"startPTS", f.pts48)
		}

		copy(framePCM, f.data)

		// Apply volume scaling (S16LE samples)
		vol := p.GetVolume()
		if vol != 100 {
			applyVolumeS16LE(framePCM, vol)
		}

		outPkt = outPkt[:0]
		if err := sess.enc.EncodeFrame(framePCM, func(pkt []byte) error {
			outPkt = append(outPkt, pkt...)
			return nil
		}); err != nil {
			return
		}
		if len(outPkt) == 0 {
			continue
		}

		offset := time.Duration((f.pts48 - media0) * int64(time.Second) / 48000)
		target := wall0.Add(offset)

		// Push to buffer
		pushAttempts := 0
		for {
			if sess.buf.Push(outPkt, f.pts48, target) {
				break
			}
			pushAttempts++
			if pushAttempts > 100 {
				slog.Warn("buffer full, dropping packet", "guildID", p.guildID)
				break
			}
			pushBackoff.Reset(10 * time.Millisecond)
			select {
			case <-sess.ctx.Done():
				if !pushBackoff.Stop() {
					select {
					case <-pushBackoff.C:
					default:
					}
				}
				return
			case <-pushBackoff.C:
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
	defer p.handlePlaybackEnd(sess, i)

	const minBufferPackets = 20

	// Reusable timers to avoid per-frame heap allocations (hot path: 50/sec)
	waitTimer := time.NewTimer(0)
	if !waitTimer.Stop() {
		<-waitTimer.C
	}
	defer waitTimer.Stop()

	dropTimer := time.NewTimer(0)
	if !dropTimer.Stop() {
		<-dropTimer.C
	}
	defer dropTimer.Stop()

	// Initial buffer wait
	for sess.buf.BufferedCount() < minBufferPackets {
		waitTimer.Reset(50 * time.Millisecond)
		select {
		case <-sess.ctx.Done():
			if !waitTimer.Stop() {
				select {
				case <-waitTimer.C:
				default:
				}
			}
			return
		case <-waitTimer.C:
		}
	}

	updatePosition := func(pts48 int64) {
		sec := int(pts48 / 48000)
		p.mu.Lock()
		if p.curPlay == sess {
			p.positionSec.Store(int32(sec))
		}
		p.mu.Unlock()
	}

	firstPacket := true
	droppedCount := 0
	packetCount := 0

	for {
		pkt, ok := sess.buf.Pop(sess.ctx, waitTimer)
		if !ok {
			break
		}

		if firstPacket {
			firstPacket = false
			updatePosition(pkt.pts48)
			slog.Info("playback started", "guildID", p.guildID, "startPTS", pkt.pts48)
		}

		if d := time.Until(pkt.targetTS); d > 0 {
			waitTimer.Reset(d)
			select {
			case <-sess.ctx.Done():
				if !waitTimer.Stop() {
					select {
					case <-waitTimer.C:
					default:
					}
				}
				sess.buf.Release(pkt.data)
				return
			case <-waitTimer.C:
			}
		}

		dropTimer.Reset(200 * time.Millisecond)
		select {
		case <-sess.ctx.Done():
			sess.buf.Release(pkt.data)
			if !dropTimer.Stop() {
				select {
				case <-dropTimer.C:
				default:
				}
			}
			return
		case vc.OpusSend <- pkt.data:
			if !dropTimer.Stop() {
				select {
				case <-dropTimer.C:
				default:
				}
			}
			updatePosition(pkt.pts48)
			droppedCount = 0
			packetCount++
			// Periodically re-send Speaking(true) every 500 packets (10 seconds)
			// to ensure Discord knows we are speaking even if discordgo reconnected
			// and silently lost its VoiceServer speaking state.
			if packetCount%500 == 0 {
				go func() { _ = vc.Speaking(true) }()
			}
		case <-dropTimer.C:
			sess.buf.Release(pkt.data)
			droppedCount++
			slog.Debug("dropped packet",
				"guildID", p.guildID,
				"consecutive", droppedCount)
		}

		buffered := sess.buf.BufferedCount()
		if buffered < 5 && buffered > 0 {
			slog.Debug("buffer running low", "count", buffered, "guildID", p.guildID)
		}
	}
}

func (p *Player) handlePlaybackEnd(sess *playSession, i *discordgo.InteractionCreate) {
	p.mu.Lock()
	if p.curPlay != sess {
		p.mu.Unlock()
		return
	}

	p.curPlay = nil
	p.positionSec.Store(0)

	var hasNext bool

	if p.loopSong.Load() {
		seek0 := 0
		p.requestedSeek = &seek0
		hasNext = true
	} else if p.shuffleMode.Load() && len(p.SongQueue) > (p.Qpos+1) {
		remainingCount := len(p.SongQueue) - (p.Qpos + 1)
		randomIndex := (p.Qpos + 1) + rand.IntN(remainingCount)

		p.SongQueue[p.Qpos+1], p.SongQueue[randomIndex] = p.SongQueue[randomIndex], p.SongQueue[p.Qpos+1]

		p.Qpos++
		hasNext = true
	} else if p.loopQueue.Load() && len(p.SongQueue) > 0 {
		p.Qpos = (p.Qpos + 1) % len(p.SongQueue)
		hasNext = true
	} else {
		p.Qpos++
		hasNext = p.Qpos < len(p.SongQueue)
		// Compact: trim already-played songs to prevent unbounded queue growth
		// (especially important during radio mode which runs indefinitely).
		if p.Qpos > 32 {
			trimmed := p.Qpos
			p.SongQueue = p.SongQueue[trimmed:]
			p.Qpos = 0
			if p.RadioQueuedIndex >= 0 {
				p.RadioQueuedIndex -= trimmed
				if p.RadioQueuedIndex < 0 {
					p.RadioQueuedIndex = -1
				}
			}
		}
	}

	// If no next song, wait for any in-flight background radio search before giving up.
	// This eliminates the gap between songs when radio pre-queued while the last song was playing.
	if !hasNext && p.IsRadioMode() && p.NowPlaying != nil {
		ch := p.radioSearchDone
		p.mu.Unlock()
		if ch != nil {
			select {
			case <-ch:
			case <-time.After(15 * time.Second):
			}
		}
		p.mu.Lock()
		hasNext = p.Qpos < len(p.SongQueue)
	}

	if !hasNext {
		if p.IsRadioMode() && p.NowPlaying != nil {
			p.mu.Unlock()
			p.tryQueueRadioSong(true)
			return
		}

		shouldSchedule := p.setIdleStateLocked()
		p.mu.Unlock()

		if shouldSchedule {
			slog.Info("playback ended, no next song - scheduling disconnect",
				"guildID", p.guildID)
			p.scheduleIdleDisconnect()
		}
		return
	}

	p.status.Store(int32(StatusIdle))
	p.mu.Unlock()

	if err := p.Play(context.Background(), nil, nil); err != nil {
		slog.Error("failed to play next song after playback end",
			"guildID", p.guildID,
			"error", err)
		p.setIdleState()
	}
}

// tryQueueRadioSong attempts to find and queue a related song for radio mode.
// If playAfter is true, it also starts playback (used when called from handlePlaybackEnd).
func (p *Player) tryQueueRadioSong(playAfter bool) {
	p.mu.Lock()
	if !p.IsRadioMode() || p.NowPlaying == nil {
		p.mu.Unlock()
		return
	}
	currentSong := *p.NowPlaying
	ctx := p.opCtx
	p.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}

	slog.Info("radio: searching for related song", "guildID", p.guildID, "current", currentSong.Title)

	related, err := p.FindRelatedSong(ctx, currentSong)
	if err != nil {
		slog.Error("radio: failed to find related song", "guildID", p.guildID, "error", err)
		p.SendNowPlayingEmbed()
		if playAfter {
			p.setIdleState()
		}
		return
	}

	related.IsRadioSuggestion = true
	related.RequestedBy = "Radio"

	p.mu.Lock()
	if !p.IsRadioMode() {
		p.mu.Unlock()
		return
	}

	p.addToRadioHistory(related.VideoID, related.Title, related.Artist)
	p.SongQueue = append(p.SongQueue, *related)
	p.RadioQueuedIndex = len(p.SongQueue) - 1

	p.mu.Unlock()

	p.SendNowPlayingEmbed()

	slog.Info("radio: queued related song", "guildID", p.guildID, "title", related.Title, "artist", related.Artist)

	if playAfter {
		if err := p.Play(ctx, nil, nil); err != nil {
			slog.Error("radio: failed to play queued song", "guildID", p.guildID, "error", err)
			p.setIdleState()
		}
	}
}
