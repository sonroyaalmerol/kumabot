package repository

import (
	"context"
	"strings"
)

type FavoritesService struct {
	repo *Repo
}

func NewFavoritesService(repo *Repo) *FavoritesService {
	return &FavoritesService{repo: repo}
}

func (f *FavoritesService) Create(ctx context.Context, guild, author, name, query string) error {
	name = strings.TrimSpace(name)
	query = strings.TrimSpace(query)
	return f.repo.AddFavorite(ctx, &Favorite{
		GuildID: guild, Author: author, Name: name, Query: query,
	})
}

func (f *FavoritesService) Remove(ctx context.Context, guild, name string) (int64, error) {
	return f.repo.RemoveFavorite(ctx, guild, strings.TrimSpace(name))
}

func (f *FavoritesService) Use(ctx context.Context, guild, name string) (*Favorite, error) {
	return f.repo.FindFavorite(ctx, guild, strings.TrimSpace(name))
}

func (f *FavoritesService) List(ctx context.Context, guild string) ([]Favorite, error) {
	return f.repo.ListFavorites(ctx, guild)
}
