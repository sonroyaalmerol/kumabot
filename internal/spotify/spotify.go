package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/zmb3/spotify/v2"
	spotifyauth "github.com/zmb3/spotify/v2/auth"
	"golang.org/x/oauth2/clientcredentials"
)

type Track struct {
	Name   string
	Artist string
}

type PlaylistMeta struct {
	Title  string
	Source string
}

type Client struct {
	raw *spotify.Client
}

func NewClientCredentials(clientID, clientSecret string) (*Client, error) {
	cfg := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     spotifyauth.TokenURL,
	}
	httpClient := cfg.Client(context.Background())
	cl := spotify.New(httpClient, spotify.WithRetry(true))
	return &Client{raw: cl}, nil
}

func (c *Client) Raw() *spotify.Client { return c.raw }

func ParseID(raw string) (typ string, id spotify.ID, err error) {
	if strings.HasPrefix(raw, "spotify:") {
		parts := strings.Split(raw, ":")
		if len(parts) == 3 {
			return parts[1], spotify.ID(parts[2]), nil
		}
		return "", "", fmt.Errorf("invalid spotify URI")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if u.Host != "open.spotify.com" && u.Host != "www.open.spotify.com" {
		return "", "", fmt.Errorf("not a spotify URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid spotify URL path")
	}
	switch parts[0] {
	case "album", "playlist", "track", "artist":
		return parts[0], spotify.ID(parts[1]), nil
	}
	return "", "", fmt.Errorf("unsupported spotify type")
}

func (c *Client) GetAlbum(ctx context.Context, id spotify.ID, limit int) ([]Track, PlaylistMeta, error) {
	alb, err := c.raw.GetAlbum(ctx, id)
	if err != nil {
		return nil, PlaylistMeta{}, err
	}
	page, err := c.raw.GetAlbumTracks(ctx, id)
	if err != nil {
		return nil, PlaylistMeta{}, err
	}
	out := make([]Track, 0, page.Total)
	add := func(items []spotify.SimpleTrack) {
		for _, t := range items {
			if limit > 0 && len(out) >= limit {
				break
			}
			artist := ""
			if len(t.Artists) > 0 {
				artist = t.Artists[0].Name
			}
			out = append(out, Track{Name: t.Name, Artist: artist})
		}
	}
	add(page.Tracks)
	for page.Next != "" && (limit == 0 || len(out) < limit) {
		if err := c.raw.NextPage(ctx, page); err != nil {
			break
		}
		add(page.Tracks)
	}
	meta := PlaylistMeta{Title: alb.Name, Source: alb.ExternalURLs["spotify"]}
	return out, meta, nil
}

func (c *Client) GetPlaylist(ctx context.Context, id spotify.ID, limit int) ([]Track, PlaylistMeta, error) {
	pl, err := c.raw.GetPlaylist(ctx, id)
	if err != nil {
		return nil, PlaylistMeta{}, err
	}
	page, err := c.raw.GetPlaylistItems(ctx, id)
	if err != nil {
		return nil, PlaylistMeta{}, err
	}
	out := make([]Track, 0, page.Total)
	add := func(items []spotify.PlaylistItem) {
		for _, it := range items {
			if it.Track.Track != nil {
				t := it.Track.Track
				if limit > 0 && len(out) >= limit {
					break
				}
				artist := ""
				if len(t.Artists) > 0 {
					artist = t.Artists[0].Name
				}
				out = append(out, Track{Name: t.Name, Artist: artist})
			}
		}
	}
	add(page.Items)
	for page.Next != "" && (limit == 0 || len(out) < limit) {
		if err := c.raw.NextPage(ctx, page); err != nil {
			break
		}
		add(page.Items)
	}
	meta := PlaylistMeta{Title: pl.Name, Source: pl.ExternalURLs["spotify"]}
	return out, meta, nil
}

func (c *Client) GetTrack(ctx context.Context, id spotify.ID) (Track, error) {
	t, err := c.raw.GetTrack(ctx, id)
	if err != nil {
		return Track{}, err
	}
	artist := ""
	if len(t.Artists) > 0 {
		artist = t.Artists[0].Name
	}
	return Track{Name: t.Name, Artist: artist}, nil
}

func (c *Client) GetArtistTop(ctx context.Context, id spotify.ID, market string, limit int) ([]Track, error) {
	full, err := c.raw.GetArtistsTopTracks(ctx, id, market)
	if err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(full))
	for _, t := range full {
		if limit > 0 && len(out) >= limit {
			break
		}
		artist := ""
		if len(t.Artists) > 0 {
			artist = t.Artists[0].Name
		}
		out = append(out, Track{Name: t.Name, Artist: artist})
	}
	return out, nil
}

func (c *Client) SearchAlbumsAndTracks(ctx context.Context, query string, limit int) ([]spotify.SimpleAlbum, []spotify.FullTrack, error) {
	if limit <= 0 {
		limit = 10
	}
	typ := spotify.SearchTypeAlbum | spotify.SearchTypeTrack
	res, err := c.raw.Search(ctx, query, typ)
	if err != nil {
		return nil, nil, err
	}
	albums := res.Albums.Albums
	if len(albums) > limit {
		albums = albums[:limit]
	}
	tracks := res.Tracks.Tracks
	if len(tracks) > limit {
		tracks = tracks[:limit]
	}
	return albums, tracks, nil
}
