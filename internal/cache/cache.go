package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
)

type FileCache struct {
	cfg  *config.Config
	repo *repository.Repo
	mu   sync.Mutex
}

func NewFileCache(cfg *config.Config, repo *repository.Repo) *FileCache {
	return &FileCache{cfg: cfg, repo: repo}
}

func (c *FileCache) HashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (c *FileCache) PathFor(hash string) string {
	return filepath.Join(c.cfg.CacheDir, hash)
}

func (c *FileCache) Get(ctx context.Context, hash string) (string, bool) {
	p := c.PathFor(hash)
	if _, err := os.Stat(p); err == nil {
		_ = c.repo.CacheTouch(ctx, hash, 0, false)
		return p, true
	}
	_ = c.repo.CacheRemove(ctx, hash)
	return "", false
}

func (c *FileCache) CreateTemp(hash string) (*os.File, string, error) {
	tmp := filepath.Join(c.cfg.CacheDir, "tmp", hash)
	f, err := os.Create(tmp)
	return f, tmp, err
}

func (c *FileCache) Commit(ctx context.Context, tmp, finalPath, hash string) error {
	info, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		_ = os.Remove(tmp)
		return nil
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		return err
	}
	_ = c.repo.CacheTouch(ctx, hash, info.Size(), true)
	return c.evictIfNeeded(ctx)
}

func (c *FileCache) evictIfNeeded(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	total, err := c.repo.CacheTotalBytes(ctx)
	if err != nil {
		return err
	}
	for total > c.cfg.CacheLimitBytes {
		oldest, err := c.repo.CacheOldest(ctx)
		if err != nil {
			return err
		}
		p := c.PathFor(oldest)
		_ = os.Remove(p)
		_ = c.repo.CacheRemove(ctx, oldest)
		total, err = c.repo.CacheTotalBytes(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *FileCache) WriteStream(ctx context.Context, key string, src io.Reader) (string, error) {
	hash := c.HashKey(key)
	final := c.PathFor(hash)
	if _, ok := c.Get(ctx, hash); ok {
		return final, nil
	}
	f, tmp, err := c.CreateTemp(hash)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := c.Commit(ctx, tmp, final, hash); err != nil {
		return "", err
	}
	return final, nil
}
