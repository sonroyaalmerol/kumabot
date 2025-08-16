package stream

import (
	"context"
	"encoding/json"

	"github.com/sonroyaalmerol/kumabot/internal/utils"
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

func YtdlpPlaylist(ctx context.Context, ytdlpPath, url string) ([]*YTDLPInfo, error) {
	// Use -J to get entries; convert each item into YTDLPInfo-like
	cmd := utils.ExecWith(ctx, ytdlpPath, "-J", "--flat-playlist", url)
	out, err := utils.CmdCombinedOutput(cmd)
	if err != nil {
		return nil, err
	}
	var pl ytdlpPlaylistJSON
	if err := json.Unmarshal(out, &pl); err != nil {
		return nil, err
	}
	var infos []*YTDLPInfo
	for _, e := range pl.Entries {
		infos = append(infos, &YTDLPInfo{
			Id: e.Id, Title: e.Title, Uploader: e.Uploader, Duration: e.Duration, IsLive: e.IsLive,
		})
	}
	return infos, nil
}
