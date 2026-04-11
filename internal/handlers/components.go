package handlers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	plib "github.com/sonroyaalmerol/kumabot/internal/player"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
)

// Component custom IDs
const (
	btnPrev      = "kuma:prev"
	btnPause     = "kuma:pause"
	btnNext      = "kuma:next"
	btnLoop      = "kuma:loop"
	btnStop      = "kuma:stop"
	btnRadio     = "kuma:radio"
	btnQueuePrev = "kuma:qprev"
	btnQueueNext = "kuma:qnext"
)

type ComponentHandler struct {
	cfg   *config.Config
	repo  *repository.Repo
	cache *cache.FileCache
	pm    *plib.PlayerManager
}

func NewComponentHandler(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache, pm *plib.PlayerManager) *ComponentHandler {
	return &ComponentHandler{cfg: cfg, repo: repo, cache: cache, pm: pm}
}

func (c *ComponentHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}
	customID := i.MessageComponentData().CustomID
	guildID := i.GuildID

	switch customID {
	case btnPrev:
		c.handlePrev(s, i, guildID)
	case btnPause:
		c.handlePause(s, i, guildID)
	case btnNext:
		c.handleNext(s, i, guildID)
	case btnLoop:
		c.handleLoop(s, i, guildID)
	case btnStop:
		c.handleStop(s, i, guildID)
	case btnRadio:
		c.handleRadio(s, i, guildID)
	case btnQueuePrev:
		c.handleQueuePage(s, i, guildID, -1)
	case btnQueueNext:
		c.handleQueuePage(s, i, guildID, 1)
	default:
		slog.Debug("unknown component", "id", customID, "guildID", guildID)
	}
}

func (c *ComponentHandler) player(guildID string) *plib.Player {
	return c.pm.Peek(guildID)
}

func respondEmpty(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
}

func respondUpdateEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
		},
	})
}

func (c *ComponentHandler) handlePrev(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) {
	p := c.player(guildID)
	if p == nil {
		respondEmpty(s, i)
		return
	}
	respondEmpty(s, i)
	_ = p.Back(context.Background(), s, i)
	p.SendNowPlayingEmbed()
}

func (c *ComponentHandler) handlePause(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) {
	p := c.player(guildID)
	if p == nil {
		respondEmpty(s, i)
		return
	}

	status := p.StatusPub()

	if status == plib.StatusPlaying {
		_ = p.Pause()
	} else if status == plib.StatusPaused {
		_ = p.Resume(context.Background(), s, i)
	}

	embed := plib.BuildPlayingEmbed(p)
	respondUpdateEmbed(s, i, embed, plib.PlayingComponents(p))
	p.UpdateVoiceChannelStatus()
}

func (c *ComponentHandler) handleNext(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) {
	p := c.player(guildID)
	if p == nil {
		respondEmpty(s, i)
		return
	}
	respondEmpty(s, i)
	_ = p.Next(context.Background(), s, i)
	p.SendNowPlayingEmbed()
}

func (c *ComponentHandler) handleLoop(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) {
	p := c.player(guildID)
	if p == nil {
		respondEmpty(s, i)
		return
	}
	p.ToggleLoopSong()
	embed := plib.BuildPlayingEmbed(p)
	respondUpdateEmbed(s, i, embed, plib.PlayingComponents(p))
}

func (c *ComponentHandler) handleStop(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) {
	p := c.player(guildID)
	if p == nil {
		respondEmpty(s, i)
		return
	}
	p.Stop()
	respondEmpty(s, i)
}

func (c *ComponentHandler) handleRadio(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string) {
	p := c.player(guildID)
	if p == nil {
		respondEmpty(s, i)
		return
	}

	on := p.ToggleRadioMode()
	if on {
		go p.TryStartRadio()
	}

	embed := plib.BuildPlayingEmbed(p)
	respondUpdateEmbed(s, i, embed, plib.PlayingComponents(p))
}

func (c *ComponentHandler) handleQueuePage(s *discordgo.Session, i *discordgo.InteractionCreate, guildID string, delta int) {
	// Extract current page from embed footer
	currentPage := 1
	for _, e := range i.Message.Embeds {
		if e.Footer != nil {
			fmt.Sscanf(e.Footer.Text, "Page %d", &currentPage)
		}
	}
	newPage := currentPage + delta
	if newPage < 1 {
		newPage = 1
	}

	ctx := context.Background()
	_, _ = c.repo.UpsertSettings(ctx, guildID)
	set, err := c.repo.GetSettings(ctx, guildID)
	if err != nil {
		respondEmpty(s, i)
		return
	}
	pageSize := set.DefaultQueuePageSize

	p := c.player(guildID)
	embed, err := plib.BuildQueueEmbed(p, newPage, pageSize)
	if err != nil {
		respondEmpty(s, i)
		return
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: plib.QueueComponents(newPage, embed),
		},
	})
}
