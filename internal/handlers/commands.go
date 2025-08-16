package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/autocomplete"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	plib "github.com/sonroyaalmerol/kumabot/internal/player"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
	"github.com/sonroyaalmerol/kumabot/internal/spotify"
	"github.com/sonroyaalmerol/kumabot/internal/ui"
	"github.com/sonroyaalmerol/kumabot/internal/utils"
)

type CommandHandler struct {
	cfg   *config.Config
	repo  *repository.Repo
	cache *cache.FileCache
	pm    *plib.PlayerManager
	favs  *repository.FavoritesService
}

func NewCommandHandler(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache, pm *plib.PlayerManager, favs *repository.FavoritesService) *CommandHandler {
	return &CommandHandler{cfg: cfg, repo: repo, cache: cache, pm: pm, favs: favs}
}

func (h *CommandHandler) RegisterCommands(s *discordgo.Session, appID string, guildID string) error {
	cmds := []*discordgo.ApplicationCommand{
		{
			Name:        "play",
			Description: "Play a song (YouTube URL/ID, HLS URL, or search)",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "query", Description: "query or URL", Type: discordgo.ApplicationCommandOptionString, Required: true},
				{Name: "immediate", Description: "add to front of queue", Type: discordgo.ApplicationCommandOptionBoolean},
				{Name: "shuffle", Description: "shuffle additions", Type: discordgo.ApplicationCommandOptionBoolean},
				{Name: "split", Description: "split chapters", Type: discordgo.ApplicationCommandOptionBoolean},
				{Name: "skip", Description: "skip current track", Type: discordgo.ApplicationCommandOptionBoolean},
			},
		},
		{Name: "stop", Description: "Stop playback and clear queue"},
		{Name: "disconnect", Description: "Pause and disconnect"},
		{Name: "clear", Description: "Clear queue except current"},
		{Name: "now-playing", Description: "Show currently playing"},
		{
			Name:        "fseek",
			Description: "Seek forward in current song",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "time", Description: "seconds or 1m30s", Type: discordgo.ApplicationCommandOptionString, Required: true},
			},
		},
		{
			Name:        "favorites",
			Description: "Manage favorites",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "use",
					Description: "use a favorite",
					Options: []*discordgo.ApplicationCommandOption{
						{Name: "name", Description: "favorite name", Type: discordgo.ApplicationCommandOptionString, Required: true},
						{Name: "immediate", Description: "front of queue", Type: discordgo.ApplicationCommandOptionBoolean},
						{Name: "shuffle", Description: "shuffle", Type: discordgo.ApplicationCommandOptionBoolean},
						{Name: "split", Description: "split chapters", Type: discordgo.ApplicationCommandOptionBoolean},
						{Name: "skip", Description: "skip current", Type: discordgo.ApplicationCommandOptionBoolean},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "list",
					Description: "list favorites",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "create",
					Description: "create favorite",
					Options: []*discordgo.ApplicationCommandOption{
						{Name: "name", Description: "name", Type: discordgo.ApplicationCommandOptionString, Required: true},
						{Name: "query", Description: "query", Type: discordgo.ApplicationCommandOptionString, Required: true},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "remove",
					Description: "remove favorite",
					Options: []*discordgo.ApplicationCommandOption{
						{Name: "name", Description: "name", Type: discordgo.ApplicationCommandOptionString, Required: true},
					},
				},
			},
		},
		{
			Name:        "config",
			Description: "Configure bot settings",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "get", Description: "show settings"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-playlist-limit", Description: "set max playlist add", Options: []*discordgo.ApplicationCommandOption{
					{Name: "limit", Description: "max tracks", Type: discordgo.ApplicationCommandOptionInteger, Required: true},
				}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-wait-after-queue-empties", Description: "time to wait before leaving VC", Options: []*discordgo.ApplicationCommandOption{
					{Name: "delay", Description: "seconds (0 never leave)", Type: discordgo.ApplicationCommandOptionInteger, Required: true},
				}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-leave-if-no-listeners", Description: "leave when no listeners", Options: []*discordgo.ApplicationCommandOption{
					{Name: "value", Description: "true/false", Type: discordgo.ApplicationCommandOptionBoolean, Required: true},
				}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-queue-add-response-hidden", Description: "ephemeral queue add responses", Options: []*discordgo.ApplicationCommandOption{
					{Name: "value", Description: "true/false", Type: discordgo.ApplicationCommandOptionBoolean, Required: true},
				}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-auto-announce-next-song", Description: "auto announce next", Options: []*discordgo.ApplicationCommandOption{
					{Name: "value", Description: "true/false", Type: discordgo.ApplicationCommandOptionBoolean, Required: true},
				}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-default-volume", Description: "default volume", Options: []*discordgo.ApplicationCommandOption{
					{Name: "level", Description: "0-100", Type: discordgo.ApplicationCommandOptionInteger, Required: true},
				}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set-default-queue-page-size", Description: "queue page size", Options: []*discordgo.ApplicationCommandOption{
					{Name: "page_size", Description: "1-30", Type: discordgo.ApplicationCommandOptionInteger, Required: true},
				}},
			},
		},
		{Name: "loop", Description: "toggle looping the current song"},
		{Name: "loop-queue", Description: "toggle looping the entire queue"},
		{
			Name:        "move",
			Description: "move songs within the queue",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "from", Description: "position of the song to move", Type: discordgo.ApplicationCommandOptionInteger, Required: true},
				{Name: "to", Description: "position to move the song to", Type: discordgo.ApplicationCommandOptionInteger, Required: true},
			},
		},
		{Name: "next", Description: "skip to the next song"},
		{Name: "pause", Description: "pause the current song"},
		{
			Name:        "queue",
			Description: "show the current queue",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "page", Description: "page of queue to show [default: 1]", Type: discordgo.ApplicationCommandOptionInteger},
				{Name: "page-size", Description: "how many items per page [default: 10, max: 30]", Type: discordgo.ApplicationCommandOptionInteger},
			},
		},
		{
			Name:        "remove",
			Description: "remove songs from the queue",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "position", Description: "position of the song to remove [default: 1]", Type: discordgo.ApplicationCommandOptionInteger},
				{Name: "range", Description: "number of songs to remove [default: 1]", Type: discordgo.ApplicationCommandOptionInteger},
			},
		},
		{Name: "replay", Description: "replay the current song"},
		{Name: "resume", Description: "resume playback"},
		{Name: "unskip", Description: "go back in the queue by one song"},
	}
	scopeGuild := guildID
	if scopeGuild == "" {
		// register globally
	}
	for _, c := range cmds {
		if _, err := s.ApplicationCommandCreate(appID, scopeGuild, c); err != nil {
			return err
		}
	}
	return nil
}

func (h *CommandHandler) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		h.handleChatCommand(s, i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		h.handleAutocomplete(s, i)
	}
}

func (h *CommandHandler) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.Name != "play" {
		return
	}

	var query string
	for _, opt := range data.Options {
		if opt.Focused {
			query = opt.StringValue()
			break
		}
		if opt.Name == "query" {
			query = opt.StringValue()
		}
	}
	if strings.TrimSpace(query) == "" {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: []*discordgo.ApplicationCommandOptionChoice{}},
		})
		return
	}

	var spClient *spotify.Client
	if h.cfg.SpotifyClientID != "" && h.cfg.SpotifyClientSecret != "" {
		client, err := spotify.NewClientCredentials(h.cfg.SpotifyClientID, h.cfg.SpotifyClientSecret)
		if err == nil {
			spClient = client
		}
	}

	choices, _ := autocomplete.GetYouTubeAndSpotifySuggestions(context.Background(), query, spClient, 10)
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{
			Choices: choices,
		},
	})
}

func (h *CommandHandler) handleChatCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	switch data.Name {
	case "play":
		h.cmdPlay(s, i)
	case "stop":
		h.cmdStop(s, i)
	case "disconnect":
		h.cmdDisconnect(s, i)
	case "clear":
		h.cmdClear(s, i)
	case "now-playing":
		h.cmdNowPlaying(s, i)
	case "fseek":
		h.cmdFseek(s, i)
	case "favorites":
		h.cmdFavorites(s, i)
	case "config":
		h.cmdConfig(s, i)
	case "loop":
		h.cmdLoop(s, i)
	case "loop-queue":
		h.cmdLoopQueue(s, i)
	case "move":
		h.cmdMove(s, i)
	case "next":
		h.cmdNext(s, i)
	case "pause":
		h.cmdPause(s, i)
	case "queue":
		h.cmdQueue(s, i)
	case "remove":
		h.cmdRemove(s, i)
	case "replay":
		h.cmdReplay(s, i)
	case "resume":
		h.cmdResume(s, i)
	case "unskip":
		h.cmdUnskip(s, i)
	}
}

func (h *CommandHandler) reply(s *discordgo.Session, i *discordgo.InteractionCreate, content string, ephemeral bool) {
	flags := uint64(0)
	if ephemeral {
		flags = 1 << 6
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlags(flags),
		},
	})
}

func (h *CommandHandler) deferReply(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) {
	flags := uint64(0)
	if ephemeral {
		flags = 1 << 6
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlags(flags),
		},
	})
}

func (h *CommandHandler) editReply(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
	})
}

func userInVoice(s *discordgo.Session, guildID, userID string) (channelID string, ok bool) {
	g, _ := s.State.Guild(guildID)
	if g == nil {
		g, _ = s.Guild(guildID)
	}
	if g == nil {
		return "", false
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID == userID && vs.ChannelID != "" {
			return vs.ChannelID, true
		}
	}
	return "", false
}

func (h *CommandHandler) enqueueAndMaybeStart(
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	query string,
	immediate bool,
	shuffleAdd bool,
	split bool,
	skip bool,
) {
	guildID := i.GuildID
	memberID := i.Member.User.ID

	chID, ok := userInVoice(s, guildID, memberID)
	if !ok {
		h.reply(s, i, "gotta be in a voice channel", true)
		return
	}

	ctx := context.Background()
	_, _ = h.repo.UpsertSettings(ctx, guildID)
	set, _ := h.repo.GetSettings(ctx, guildID)

	h.deferReply(s, i, set.QAddEphemeral)

	player := h.pm.Get(h.cfg, h.repo, h.cache, guildID)
	if err := player.Connect(s, guildID, chID); err != nil {
		h.editReply(s, i, "couldn't connect to channel")
		return
	}

	newSongs, extraMsg, err := plib.ResolveQueryToSongs(ctx, h.cfg, query, set.PlaylistLimit, split)
	if err != nil || len(newSongs) == 0 {
		h.editReply(s, i, "no songs found")
		return
	}

	if shuffleAdd && len(newSongs) > 1 {
		utils.ShuffleSlice(newSongs)
	}

	for idx := range newSongs {
		newSongs[idx].RequestedBy = memberID
		newSongs[idx].AddedInChan = i.ChannelID
		player.Add(newSongs[idx], immediate)
	}

	wasIdle := player.Status == plib.StatusIdle
	if skip {
		_ = player.Forward(ctx, s, 1)
	}

	if wasIdle {
		if err := player.Play(ctx, s); err != nil {
			h.editReply(s, i, "failed to start playback")
			return
		}
	}

	first := newSongs[0]
	msg := ""
	if len(newSongs) == 1 {
		msg = fmt.Sprintf("u betcha, %s added to the%s queue%s%s",
			utils.EscapeMd(first.Title),
			func() string {
				if immediate {
					return " front of the"
				}
				return ""
			}(),
			func() string {
				if skip {
					return " and current track skipped"
				}
				return ""
			}(),
			func() string {
				if extraMsg != "" {
					return " (" + extraMsg + ")"
				}
				return ""
			}(),
		)
	} else {
		msg = fmt.Sprintf("u betcha, %s and %d other songs were added to the queue%s%s",
			utils.EscapeMd(first.Title),
			len(newSongs)-1,
			func() string {
				if skip {
					return " and current track skipped"
				}
				return ""
			}(),
			func() string {
				if extraMsg != "" {
					return " (" + extraMsg + ")"
				}
				return ""
			}(),
		)
	}
	h.editReply(s, i, msg)
}

func (h *CommandHandler) cmdPlay(s *discordgo.Session, i *discordgo.InteractionCreate) {
	var query string
	var immediate, shuffleAdd, split, skip bool
	for _, o := range i.ApplicationCommandData().Options {
		switch o.Name {
		case "query":
			query = o.StringValue()
		case "immediate":
			immediate = o.BoolValue()
		case "shuffle":
			shuffleAdd = o.BoolValue()
		case "split":
			split = o.BoolValue()
		case "skip":
			skip = o.BoolValue()
		}
	}
	h.enqueueAndMaybeStart(s, i, query, immediate, shuffleAdd, split, skip)
}

func (h *CommandHandler) cmdStop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if player == nil || player.Conn == nil {
		h.reply(s, i, "not connected", true)
		return
	}
	if player.Status != plib.StatusPlaying {
		h.reply(s, i, "not currently playing", true)
		return
	}
	player.Stop()
	h.reply(s, i, "u betcha, stopped", false)
}

func (h *CommandHandler) cmdDisconnect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if player == nil || player.Conn == nil {
		h.reply(s, i, "not connected", true)
		return
	}
	player.Disconnect()
	h.reply(s, i, "u betcha, disconnected", false)
}

func (h *CommandHandler) cmdClear(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	player.Clear()
	h.reply(s, i, "clearer than a field after a fresh harvest", false)
}

func (h *CommandHandler) cmdNowPlaying(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	cur := player.GetCurrent()
	if cur == nil {
		h.reply(s, i, "nothing is currently playing", true)
		return
	}

	embed := ui.BuildPlayingEmbed(player)
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func (h *CommandHandler) cmdFseek(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	cur := player.GetCurrent()
	if cur == nil {
		h.reply(s, i, "nothing is playing", true)
		return
	}
	if cur.IsLive {
		h.reply(s, i, "can't seek in a livestream", true)
		return
	}
	var tstr string
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == "time" {
			tstr = o.StringValue()
		}
	}
	sec := utils.ParseDurationString(tstr)
	if sec <= 0 {
		h.reply(s, i, "invalid time", true)
		return
	}
	if sec+player.GetPosition() > cur.Length {
		h.reply(s, i, "can't seek past the end of the song", true)
		return
	}
	_ = player.Seek(context.Background(), s, player.GetPosition()+sec)
	h.reply(s, i, "👍 seeked to "+utils.PrettyTime(player.GetPosition()), false)
}

func (h *CommandHandler) cmdFavorites(s *discordgo.Session, i *discordgo.InteractionCreate) {
	sub := i.ApplicationCommandData().Options[0]
	ctx := context.Background()
	switch sub.Name {
	case "create":
		var name, query string
		for _, o := range sub.Options {
			if o.Name == "name" {
				name = o.StringValue()
			} else if o.Name == "query" {
				query = o.StringValue()
			}
		}
		if err := h.favs.Create(ctx, i.GuildID, i.Member.User.ID, name, query); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				h.reply(s, i, "a favorite with that name already exists", true)
				return
			}
			h.reply(s, i, "failed to create favorite", true)
			return
		}
		h.reply(s, i, "👍 favorite created", false)
	case "remove":
		var name string
		for _, o := range sub.Options {
			if o.Name == "name" {
				name = o.StringValue()
			}
		}
		f, err := h.favs.Use(ctx, i.GuildID, name)
		if err != nil {
			h.reply(s, i, "no favorite with that name exists", true)
			return
		}
		isOwner := i.Member != nil && i.Member.User != nil && (i.Member.User.ID == f.Author || i.GuildID == "")
		if !isOwner && i.Member != nil && i.Member.User.ID != f.Author {
			h.reply(s, i, "you can only remove your own favorites", true)
			return
		}
		_, _ = h.favs.Remove(ctx, i.GuildID, name)
		h.reply(s, i, "👍 favorite removed", false)
	case "list":
		items, _ := h.favs.List(ctx, i.GuildID)
		if len(items) == 0 {
			h.reply(s, i, "there aren't any favorites yet", false)
			return
		}
		var b strings.Builder
		for _, f := range items {
			b.WriteString(fmt.Sprintf("• %s: %s (<@%s>)\n", f.Name, f.Query, f.Author))
		}
		h.reply(s, i, b.String(), true)
	case "use":
		var name string
		var immediate, shuffleAdd, split, skip bool
		for _, o := range sub.Options {
			switch o.Name {
			case "name":
				name = o.StringValue()
			case "immediate":
				immediate = o.BoolValue()
			case "shuffle":
				shuffleAdd = o.BoolValue()
			case "split":
				split = o.BoolValue()
			case "skip":
				skip = o.BoolValue()
			}
		}
		f, err := h.favs.Use(ctx, i.GuildID, name)
		if err != nil {
			h.reply(s, i, "no favorite with that name exists", true)
			return
		}
		h.enqueueAndMaybeStart(s, i, f.Query, immediate, shuffleAdd, split, skip)
	}
}

func (h *CommandHandler) cmdConfig(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx := context.Background()
	_, _ = h.repo.UpsertSettings(ctx, i.GuildID)
	sub := i.ApplicationCommandData().Options[0]
	switch sub.Name {
	case "get":
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		msg := fmt.Sprintf(
			"Config\n- Playlist Limit: %d\n- Wait before leaving after queue empty: %s\n- Leave if no listeners: %t\n- Auto announce next song: %t\n- Add to queue responses ephemeral: %t\n- Default volume: %d\n- Default queue page size: %d\n- Reduce volume when people speak: %t",
			set.PlaylistLimit,
			func() string {
				if set.SecondsWaitAfterEmpty == 0 {
					return "never leave"
				} else {
					return fmt.Sprintf("%ds", set.SecondsWaitAfterEmpty)
				}
			}(),
			set.LeaveIfNoListeners,
			set.AutoAnnounceNext,
			set.QAddEphemeral,
			set.DefaultVolume,
			set.DefaultQueuePageSize,
			set.TurnDownWhenSpeaking,
		)
		h.reply(s, i, msg, false)
	case "set-playlist-limit":
		limit := int(sub.Options[0].IntValue())
		if limit < 1 {
			h.reply(s, i, "invalid limit", true)
			return
		}
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.PlaylistLimit = limit
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 limit updated", false)
	case "set-wait-after-queue-empties":
		delay := int(sub.Options[0].IntValue())
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.SecondsWaitAfterEmpty = delay
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 wait delay updated", false)
	case "set-leave-if-no-listeners":
		val := sub.Options[0].BoolValue()
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.LeaveIfNoListeners = val
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 leave setting updated", false)
	case "set-queue-add-response-hidden":
		val := sub.Options[0].BoolValue()
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.QAddEphemeral = val
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 queue add notification setting updated", false)
	case "set-auto-announce-next-song":
		val := sub.Options[0].BoolValue()
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.AutoAnnounceNext = val
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 auto announce setting updated", false)
	case "set-default-volume":
		val := int(sub.Options[0].IntValue())
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.DefaultVolume = val
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 volume setting updated", false)
	case "set-default-queue-page-size":
		val := int(sub.Options[0].IntValue())
		set, _ := h.repo.GetSettings(ctx, i.GuildID)
		set.DefaultQueuePageSize = val
		_ = h.repo.UpdateSettings(ctx, set)
		h.reply(s, i, "👍 default queue page size updated", false)
	}
}

func (h *CommandHandler) cmdLoop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if player.Status == plib.StatusIdle {
		h.reply(s, i, "no song to loop!", true)
		return
	}
	on := player.ToggleLoopSong()
	if on {
		h.reply(s, i, "looped :)", false)
	} else {
		h.reply(s, i, "stopped looping :(", false)
	}
}

func (h *CommandHandler) cmdLoopQueue(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	on, err := player.ToggleLoopQueue()
	if err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	if on {
		h.reply(s, i, "looped queue :)", false)
	} else {
		h.reply(s, i, "stopped looping queue :(", false)
	}
}

func (h *CommandHandler) cmdMove(s *discordgo.Session, i *discordgo.InteractionCreate) {
	var from, to int
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == "from" {
			from = int(o.IntValue())
		}
		if o.Name == "to" {
			to = int(o.IntValue())
		}
	}
	if from < 1 || to < 1 {
		h.reply(s, i, "position must be at least 1", true)
		return
	}
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	item, err := player.Move(from, to)
	if err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	h.reply(s, i, fmt.Sprintf("moved **%s** to position **%d**", utils.EscapeMd(item.Title), to), false)
}

func (h *CommandHandler) cmdNext(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if err := player.Next(context.Background(), s); err != nil {
		h.reply(s, i, "no song to skip to", true)
		return
	}
	h.reply(s, i, "skipped to next", false)
}

func (h *CommandHandler) cmdPause(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if player.Status != plib.StatusPlaying {
		h.reply(s, i, "not currently playing", true)
		return
	}
	if err := player.PauseCmd(); err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	h.reply(s, i, "the stop-and-go light is now red", false)
}

func (h *CommandHandler) cmdQueue(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx := context.Background()
	_, _ = h.repo.UpsertSettings(ctx, i.GuildID)
	set, _ := h.repo.GetSettings(ctx, i.GuildID)

	page := 1
	pageSize := set.DefaultQueuePageSize
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == "page" {
			page = int(o.IntValue())
		} else if o.Name == "page-size" {
			pageSize = int(o.IntValue())
			if pageSize < 1 {
				pageSize = 1
			}
			if pageSize > 30 {
				pageSize = 30
			}
		}
	}
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)

	embed, err := ui.BuildQueueEmbed(player, page, pageSize)
	if err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  1 << 6, // ephemeral if desired
		},
	})
}

func (h *CommandHandler) cmdRemove(s *discordgo.Session, i *discordgo.InteractionCreate) {
	pos := 1
	cnt := 1
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == "position" {
			pos = int(o.IntValue())
		} else if o.Name == "range" {
			cnt = int(o.IntValue())
		}
	}
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if err := player.RemoveFromQueue(pos, cnt); err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	h.reply(s, i, ":wastebasket: removed", false)
}

func (h *CommandHandler) cmdReplay(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if err := player.Replay(context.Background(), s); err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	h.reply(s, i, "👍 replayed the current song", false)
}

func (h *CommandHandler) cmdResume(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if player.Status == plib.StatusPlaying {
		h.reply(s, i, "already playing, give me a song name", true)
		return
	}
	if player.GetCurrent() == nil {
		h.reply(s, i, "nothing to play", true)
		return
	}
	if err := player.Resume(context.Background(), s); err != nil {
		h.reply(s, i, err.Error(), true)
		return
	}
	h.reply(s, i, "the stop-and-go light is now green", false)
}

func (h *CommandHandler) cmdUnskip(s *discordgo.Session, i *discordgo.InteractionCreate) {
	player := h.pm.Get(h.cfg, h.repo, h.cache, i.GuildID)
	if err := player.Back(context.Background(), s); err != nil {
		h.reply(s, i, "no song to go back to", true)
		return
	}
	// Optionally show now playing (simple text)
	cur := player.GetCurrent()
	if cur != nil {
		h.reply(s, i, fmt.Sprintf("back 'er up' — now playing %s", utils.EscapeMd(cur.Title)), false)
	} else {
		h.reply(s, i, "back 'er up'", false)
	}
}
