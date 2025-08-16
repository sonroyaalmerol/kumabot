package stream

import (
	"context"
	"fmt"
	"strings"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

type YTDLPRequestedFormat struct {
	Url string `json:"url"`
}

type YTDLPFormat struct {
	Url string `json:"url"`
}

type YTDLPEntry struct {
	Id          string `json:"id"`
	Title       string `json:"title"`
	Uploader    string `json:"uploader"`
	Duration    int    `json:"duration"`
	IsLive      bool   `json:"is_live"`
	Description string `json:"description"`
	WebpageUrl  string `json:"webpage_url"`
	Thumbnails  []struct {
		Url string `json:"url"`
	} `json:"thumbnails"`
	Formats          []YTDLPFormat          `json:"formats"`
	RequestedFormats []YTDLPRequestedFormat `json:"requested_formats"`
	Url              string                 `json:"url"`
}

type YTDLPInfo struct {
	Id          string `json:"id"`
	Title       string `json:"title"`
	Uploader    string `json:"uploader"`
	Duration    int    `json:"duration"`
	IsLive      bool   `json:"is_live"`
	Description string `json:"description"`
	WebpageUrl  string `json:"webpage_url"`
	Thumbnails  []struct {
		Url string `json:"url"`
	} `json:"thumbnails"`
	Formats          []YTDLPFormat          `json:"formats"`
	RequestedFormats []YTDLPRequestedFormat `json:"requested_formats"`
	Url              string                 `json:"url"`

	Entries []YTDLPEntry `json:"entries"`
}

// YtdlpGetInfo runs yt-dlp -J -f bestaudio/best URL.
// Use ytsearch1:query to get best single hit; for generic queries, detect playlist+entries.
func YtdlpGetInfo(ctx context.Context, url string) (*YTDLPInfo, error) {
	cmd := ytdlp.New().
		Format("bestaudio/best").
		DumpJSON()

	res, err := cmd.Run(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("yt-dlp run: %w", err)
	}

	infos, err := res.GetExtractedInfo()
	if err != nil {
		return nil, fmt.Errorf("parse yt-dlp json: %w", err)
	}
	if len(infos) == 0 || infos[0] == nil {
		return nil, fmt.Errorf("parse yt-dlp json: no info returned")
	}
	// go-ytdlp may return multiple ExtractedInfo messages; use the first as
	// the container, which can be a single item or playlist/search.
	ext := infos[0]
	out := &YTDLPInfo{}

	// Helper mappers
	mapThumbs := func(ts []*ytdlp.ExtractedThumbnail) []struct {
		Url string `json:"url"`
	} {
		if len(ts) == 0 {
			return nil
		}
		out := make([]struct {
			Url string `json:"url"`
		}, 0, len(ts))
		for _, t := range ts {
			out = append(out, struct {
				Url string `json:"url"`
			}{Url: t.URL})
		}
		return out
	}
	mapFormats := func(fs []*ytdlp.ExtractedFormat) []YTDLPFormat {
		if len(fs) == 0 {
			return nil
		}
		out := make([]YTDLPFormat, 0, len(fs))
		for _, f := range fs {
			out = append(out, YTDLPFormat{Url: f.URL})
		}
		return out
	}
	mapReqFormats := func(fs []*ytdlp.ExtractedFormat) []YTDLPRequestedFormat {
		if len(fs) == 0 {
			return nil
		}
		out := make([]YTDLPRequestedFormat, 0, len(fs))
		for _, f := range fs {
			out = append(out, YTDLPRequestedFormat{Url: f.URL})
		}
		return out
	}

	// If it's a playlist/search container, fill Entries and mirror the first into top level
	if len(ext.Entries) > 0 {
		out.Entries = make([]YTDLPEntry, 0, len(ext.Entries))
		for _, e := range ext.Entries {
			entry := YTDLPEntry{
				Id:          e.ID,
				Title:       *e.Title,
				Uploader:    *e.Uploader,
				Duration:    int(*e.Duration),
				IsLive:      *e.IsLive,
				Description: *e.Description,
				WebpageUrl:  *e.WebpageURL,
				Url:         *e.URL,
				Thumbnails:  mapThumbs(e.Thumbnails),
				Formats:     mapFormats(e.Formats),
				RequestedFormats: mapReqFormats(
					e.RequestedFormats,
				),
			}
			out.Entries = append(out.Entries, entry)
		}

		first := ext.Entries[0]
		out.Id = first.ID
		out.Title = *first.Title
		out.Uploader = *first.Uploader
		out.Duration = int(*first.Duration)
		out.IsLive = *first.IsLive
		out.Description = *first.Description
		out.WebpageUrl = *first.WebpageURL
		out.Url = *first.URL
		out.Thumbnails = mapThumbs(first.Thumbnails)
		out.Formats = mapFormats(first.Formats)
		out.RequestedFormats = mapReqFormats(first.RequestedFormats)

		return out, nil
	}

	// Single-item result
	out.Id = ext.ID
	out.Title = *ext.Title
	out.Uploader = *ext.Uploader
	out.Duration = int(*ext.Duration)
	out.IsLive = *ext.IsLive
	out.Description = *ext.Description
	out.WebpageUrl = *ext.WebpageURL
	out.Url = *ext.URL
	out.Thumbnails = mapThumbs(ext.Thumbnails)
	out.Formats = mapFormats(ext.Formats)
	out.RequestedFormats = mapReqFormats(ext.RequestedFormats)

	return out, nil
}

// YtdlpAudioURL returns the best playable URL.
// Preferred order: requested_formats (audio/video), top-level url, then formats[].
func YtdlpAudioURL(info *YTDLPInfo) string {
	if len(info.RequestedFormats) > 0 {
		for _, rf := range info.RequestedFormats {
			if strings.HasPrefix(rf.Url, "http") {
				return rf.Url
			}
		}
	}
	if info.Url != "" && strings.HasPrefix(info.Url, "http") {
		return info.Url
	}
	for _, f := range info.Formats {
		if strings.HasPrefix(f.Url, "http") {
			return f.Url
		}
	}
	if info.WebpageUrl != "" {
		return info.WebpageUrl
	}
	return ""
}
