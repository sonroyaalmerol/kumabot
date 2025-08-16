package handlers

import (
	"context"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/player"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
)

type Bot struct {
	cfg   *config.Config
	repo  *repository.Repo
	cache *cache.FileCache
	pm    *player.PlayerManager
	cmd   *CommandHandler
}

func NewBot(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache) *Bot {
	pm := player.NewPlayerManager()
	cmd := NewCommandHandler(cfg, repo, cache, pm, repository.NewFavoritesService(repo))
	return &Bot{
		cfg: cfg, repo: repo, cache: cache, pm: pm, cmd: cmd,
	}
}

func (b *Bot) Run(ctx context.Context) error {
	dg, err := discordgo.New("Bot " + b.cfg.DiscordToken)
	if err != nil {
		return err
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	// On ready: register commands depending on configuration
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		slog.Info("connected", "user", s.State.User.Username)
		appID := s.State.User.ID

		if b.cfg.RegisterCommandsOnBot {
			if err := b.cmd.RegisterCommands(s, appID, ""); err != nil {
				slog.Error("register global commands", "err", err)
			} else {
				slog.Info("registered global application commands")
			}
		} else {
			var wg sync.WaitGroup
			for _, g := range s.State.Guilds {
				wg.Add(1)
				go func(guildID string) {
					defer wg.Done()
					if err := b.cmd.RegisterCommands(s, appID, guildID); err != nil {
						slog.Error("register guild commands", "guild", guildID, "err", err)
					}
				}(g.ID)
			}
			wg.Wait()

			_, err := s.ApplicationCommandBulkOverwrite(appID, "", []*discordgo.ApplicationCommand{})
			if err != nil {
				slog.Error("clear global commands", "err", err)
			} else {
				slog.Info("cleared global application commands")
			}

			slog.Info("registered commands on all guilds")
		}
	})

	// If registering per-guild, register on new guilds too
	dg.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		if b.cfg.RegisterCommandsOnBot {
			return
		}
		appID := s.State.User.ID
		if err := b.cmd.RegisterCommands(s, appID, g.ID); err != nil {
			slog.Error("register guild commands on join", "guild", g.ID, "err", err)
		} else {
			slog.Info("registered commands on new guild", "guild", g.ID)
		}
	})

	// Interactions
	dg.AddHandler(b.cmd.HandleInteraction)

	// voice state update for leave-if-no-listeners
	dg.AddHandler(func(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		gid := vs.GuildID
		player := b.pm.Peek(gid)
		if player == nil || player.Conn == nil {
			return
		}
		set, err := b.repo.GetSettings(context.Background(), gid)
		if err != nil || set == nil || !set.LeaveIfNoListeners {
			return
		}
		// check channel size without bots
		chID := player.Conn.ChannelID
		size := getNonBotSize(s, gid, chID)
		if size == 0 {
			player.Disconnect()
		}
	})

	if err := dg.Open(); err != nil {
		return err
	}
	defer dg.Close()

	<-ctx.Done()
	return nil
}

func getNonBotSize(s *discordgo.Session, guildID, channelID string) int {
	g, _ := s.State.Guild(guildID)
	if g == nil {
		return 0
	}
	n := 0
	for _, vs := range g.VoiceStates {
		if vs.ChannelID == channelID {
			m, _ := s.State.Member(guildID, vs.UserID)
			if m != nil && m.User != nil && !m.User.Bot {
				n++
			}
		}
	}
	return n
}
