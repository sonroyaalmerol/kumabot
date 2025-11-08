package stream

import (
	"context"
	"fmt"
	"strings"

	ytdlp "github.com/lrstanley/go-ytdlp"
	"github.com/sonroyaalmerol/kumabot/internal/config"
)

type ytdlpPlaylistItem struct {
	Id         string `json:"id"`
	Title      string `json:"title"`
	Uploader   string `json:"uploader"`
	Duration   int    `json:"duration"`
	IsLive     bool   `json:"is_live"`
	Thumbnails []struct {
		Url string `json:"url"`
	} `json:"thumbnails"`
}
type ytdlpPlaylistJSON struct {
	Entries    []ytdlpPlaylistItem `json:"entries"`
	Title      string              `json:"title"`
	WebpageUrl string              `json:"webpage_url"`
}

func YtdlpPlaylist(ctx context.Context, cfg *config.Config, url string) ([]*YTDLPInfo, error) {
	cmd := ytdlp.New().
		FlatPlaylist().
		DumpJSON()

	if cfg.YouTubeCookiesPath != "" {
		cmd = cmd.Cookies(cfg.YouTubeCookiesPath)
		ytdlpDebugf("playlist: using cookies from file")
	}

	// Add YouTube-specific extractor args
	if strings.Contains(url, "youtube.com") || strings.Contains(url, "youtu.be") {
		extractorArgs := "youtube:player-client=default,mweb"
		if cfg.YouTubePOToken != "" {
			extractorArgs += ";po_token=" + cfg.YouTubePOToken
		}
		cmd = cmd.ExtractorArgs(extractorArgs)
		ytdlpDebugf("playlist fetch with YouTube extractor args")
	}

	ytdlpDebugf("fetching playlist: %s", url)
	res, err := cmd.Run(ctx, url)
	if err != nil {
		if strings.Contains(err.Error(), "Sign in to confirm") {
			return nil, fmt.Errorf("yt-dlp playlist fetch failed (PO token may be required): %w", err)
		}
		return nil, fmt.Errorf("yt-dlp playlist fetch failed for %s: %w", url, err)
	}

	infosParsed, err := res.GetExtractedInfo()
	if err != nil {
		return nil, fmt.Errorf("parse yt-dlp playlist json for %s: %w", url, err)
	}
	if len(infosParsed) == 0 || infosParsed[0] == nil {
		return nil, fmt.Errorf("yt-dlp returned empty playlist info for %s", url)
	}

	pl := infosParsed[0]
	ytdlpDebugf("playlist parsed: %d entries", len(pl.Entries))

	out := make([]*YTDLPInfo, 0, len(pl.Entries))
	for i, e := range pl.Entries {
		if e == nil {
			ytdlpDebugf("skipping nil entry at index %d", i)
			continue
		}
		out = append(out, &YTDLPInfo{
			Id:       e.ID,
			Title:    s(e.Title),
			Uploader: s(e.Uploader),
			Duration: f(e.Duration),
			IsLive:   b(e.IsLive),
		})
	}

	ytdlpDebugf("playlist processed: %d valid entries", len(out))
	return out, nil
}
