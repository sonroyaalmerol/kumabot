package autocomplete

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/spotify"
)

func GetYouTubeSuggestions(query string) ([]string, error) {
	u, _ := url.Parse("https://suggestqueries.google.com/complete/search")
	q := u.Query()
	q.Set("client", "firefox")
	q.Set("ds", "yt")
	q.Set("q", query)
	u.RawQuery = q.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed []any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	arr, ok := parsed[1].([]any)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func GetYouTubeAndSpotifySuggestions(ctx context.Context, query string, sp *spotify.Client, limit int) ([]*discordgo.ApplicationCommandOptionChoice, error) {
	if limit <= 0 {
		limit = 10
	}
	yt, _ := GetYouTubeSuggestions(query)

	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, limit)
	yts := min(len(yt), limit)
	for i := 0; i < yts; i++ {
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
			Name:  "YouTube: " + yt[i],
			Value: yt[i],
		})
	}

	if sp != nil {
		maxSP := limit / 2
		albums, tracks, err := sp.SearchAlbumsAndTracks(ctx, query, maxSP)
		if err == nil {
			// make room
			if len(out) > limit-len(albums)-len(tracks) {
				out = out[:limit-len(albums)-len(tracks)]
			}
			for _, a := range albums {
				artist := ""
				if len(a.Artists) > 0 {
					artist = a.Artists[0].Name
				}
				name := fmt.Sprintf("Spotify: 💿 %s", a.Name)
				if artist != "" {
					name += " - " + artist
				}
				out = append(out, &discordgo.ApplicationCommandOptionChoice{
					Name:  name,
					Value: "spotify:album:" + a.ID.String(),
				})
			}
			for _, t := range tracks {
				artist := ""
				if len(t.Artists) > 0 {
					artist = t.Artists[0].Name
				}
				name := fmt.Sprintf("Spotify: 🎵 %s", t.Name)
				if artist != "" {
					name += " - " + artist
				}
				out = append(out, &discordgo.ApplicationCommandOptionChoice{
					Name:  name,
					Value: "spotify:track:" + t.ID.String(),
				})
			}
		}
	}

	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
