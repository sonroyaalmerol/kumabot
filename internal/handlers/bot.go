package handlers

import (
	"context"
	"fmt"
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
		return fmt.Errorf("failed to create Discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		slog.Info("connected", "user", s.State.User.Username, "guilds", len(s.State.Guilds))
		appID := s.State.User.ID

		if b.cfg.RegisterCommandsOnBot {
			if err := b.cmd.RegisterCommands(s, appID, ""); err != nil {
				slog.Error("register global commands failed", "err", err)
			} else {
				slog.Info("registered global application commands")
			}
		} else {
			var wg sync.WaitGroup
			successCount := 0
			failCount := 0
			var mu sync.Mutex

			for _, g := range s.State.Guilds {
				wg.Add(1)
				go func(guildID string) {
					defer wg.Done()
					if err := b.cmd.RegisterCommands(s, appID, guildID); err != nil {
						slog.Error("register guild commands failed", "guild", guildID, "err", err)
						mu.Lock()
						failCount++
						mu.Unlock()
					} else {
						mu.Lock()
						successCount++
						mu.Unlock()
					}
				}(g.ID)
			}
			wg.Wait()

			slog.Info("finished registering commands on guilds", "success", successCount, "failed", failCount)

			_, err := s.ApplicationCommandBulkOverwrite(appID, "", []*discordgo.ApplicationCommand{})
			if err != nil {
				slog.Error("clear global commands failed", "err", err)
			} else {
				slog.Info("cleared global application commands")
			}
		}
	})

	dg.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		if b.cfg.RegisterCommandsOnBot {
			return
		}
		appID := s.State.User.ID
		if err := b.cmd.RegisterCommands(s, appID, g.ID); err != nil {
			slog.Error("register guild commands on join failed", "guild", g.ID, "guildName", g.Name, "err", err)
		} else {
			slog.Info("registered commands on new guild", "guild", g.ID, "guildName", g.Name)
		}
	})

	dg.AddHandler(b.cmd.HandleInteraction)

	dg.AddHandler(func(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		gid := vs.GuildID
		player := b.pm.Peek(gid)
		if player == nil || player.Conn == nil {
			return
		}
		set, err := b.repo.GetSettings(context.Background(), gid)
		if err != nil {
			slog.Warn("failed to get settings for voice state check", "guildID", gid, "err", err)
			return
		}
		if set == nil || !set.LeaveIfNoListeners {
			return
		}
		chID := player.ConnChannelID
		size := getNonBotSize(s, gid, chID)
		slog.Debug("voice state update", "guildID", gid, "channelID", chID, "nonBotCount", size)
		if size == 0 {
			slog.Info("leaving voice channel: no listeners", "guildID", gid, "channelID", chID)
			player.Disconnect()
		}
	})

	if err := dg.Open(); err != nil {
		return fmt.Errorf("failed to open Discord connection: %w", err)
	}
	defer dg.Close()

	slog.Info("bot is now running")
	<-ctx.Done()
	slog.Info("bot shutting down", "reason", ctx.Err())
	return nil
}

func getNonBotSize(s *discordgo.Session, guildID, channelID string) int {
	g, err := s.State.Guild(guildID)
	if err != nil {
		return 0
	}
	n := 0
	for _, vs := range g.VoiceStates {
		if vs.ChannelID == channelID {
			if vs.Member != nil && vs.Member.User != nil {
				if !vs.Member.User.Bot {
					n++
				}
				continue
			}
			m, err := s.State.Member(guildID, vs.UserID)
			if err == nil && m.User != nil && !m.User.Bot {
				n++
			}
		}
	}
	return n
}
