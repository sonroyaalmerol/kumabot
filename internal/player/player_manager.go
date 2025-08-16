package player

import (
	"sync"

	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
)

type PlayerManager struct {
	mu      sync.Mutex
	Players map[string]*Player
}

func NewPlayerManager() *PlayerManager {
	return &PlayerManager{Players: make(map[string]*Player)}
}

func (pm *PlayerManager) Get(cfg *config.Config, repo *repository.Repo, cache *cache.FileCache, guildID string) *Player {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if p, ok := pm.Players[guildID]; ok {
		return p
	}
	p := NewPlayer(cfg, repo, cache, guildID)
	pm.Players[guildID] = p
	return p
}

func (pm *PlayerManager) Peek(guildID string) *Player {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.Players[guildID]
}
