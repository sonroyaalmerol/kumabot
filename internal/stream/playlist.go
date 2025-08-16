package stream

import (
	"context"

	ytdlp "github.com/lrstanley/go-ytdlp"
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

func YtdlpPlaylist(ctx context.Context, url string) ([]*YTDLPInfo, error) {
	cmd := ytdlp.New().
		FlatPlaylist(). // --flat-playlist
		DumpJSON()      // -J

	res, err := cmd.Run(ctx, url)
	if err != nil {
		return nil, err
	}

	infosParsed, err := res.GetExtractedInfo()
	if err != nil {
		return nil, err
	}
	if len(infosParsed) == 0 || infosParsed[0] == nil {
		return nil, nil
	}

	pl := infosParsed[0]
	out := make([]*YTDLPInfo, 0, len(pl.Entries))
	for _, e := range pl.Entries {
		// Map the fields we care about into our YTDLPInfo shape.
		out = append(out, &YTDLPInfo{
			Id:       e.ID,
			Title:    s(e.Title),
			Uploader: s(e.Uploader),
			Duration: f(e.Duration),
			IsLive:   b(e.IsLive),
		})
	}

	return out, nil
}
