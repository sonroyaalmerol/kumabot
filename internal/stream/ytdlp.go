package stream

import (
	"context"
	"fmt"
	"strings"
	"sync"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

type YTDLPRequestedFormat struct {
	Url string `json:"url"`
}

type YTDLPFormat struct {
	Url string `json:"url"`
}

type YTDLPEntry struct {
	Id          string  `json:"id"`
	Title       string  `json:"title"`
	Uploader    string  `json:"uploader"`
	Duration    float64 `json:"duration"`
	IsLive      bool    `json:"is_live"`
	Description string  `json:"description"`
	WebpageUrl  string  `json:"webpage_url"`
	Thumbnails  []struct {
		Url string `json:"url"`
	} `json:"thumbnails"`
	Formats          []YTDLPFormat          `json:"formats"`
	RequestedFormats []YTDLPRequestedFormat `json:"requested_formats"`
	Url              string                 `json:"url"`
}

type YTDLPInfo struct {
	Id          string  `json:"id"`
	Title       string  `json:"title"`
	Uploader    string  `json:"uploader"`
	Duration    float64 `json:"duration"`
	IsLive      bool    `json:"is_live"`
	Description string  `json:"description"`
	WebpageUrl  string  `json:"webpage_url"`
	Thumbnails  []struct {
		Url string `json:"url"`
	} `json:"thumbnails"`
	Formats          []YTDLPFormat          `json:"formats"`
	RequestedFormats []YTDLPRequestedFormat `json:"requested_formats"`
	Url              string                 `json:"url"`

	Entries []YTDLPEntry `json:"entries"`
}

var (
	installOnce sync.Once
)

// helpers to safely read pointer fields with defaults
func s(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}
func f(ptr *float64) float64 {
	if ptr == nil {
		return 0
	}
	return *ptr
}
func b(ptr *bool) bool {
	if ptr == nil {
		return false
	}
	return *ptr
}

func mapThumbs(ts []*ytdlp.ExtractedThumbnail) []struct {
	Url string `json:"url"`
} {
	if len(ts) == 0 {
		return nil
	}
	out := make([]struct {
		Url string `json:"url"`
	}, 0, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		out = append(out, struct {
			Url string `json:"url"`
		}{Url: t.URL})
	}
	return out
}
func mapFormats(fs []*ytdlp.ExtractedFormat) []YTDLPFormat {
	if len(fs) == 0 {
		return nil
	}
	out := make([]YTDLPFormat, 0, len(fs))
	for _, f := range fs {
		if f == nil {
			continue
		}
		out = append(out, YTDLPFormat{Url: f.URL})
	}
	return out
}
func mapReqFormats(fs []*ytdlp.ExtractedFormat) []YTDLPRequestedFormat {
	if len(fs) == 0 {
		return nil
	}
	out := make([]YTDLPRequestedFormat, 0, len(fs))
	for _, f := range fs {
		if f == nil {
			continue
		}
		out = append(out, YTDLPRequestedFormat{Url: f.URL})
	}
	return out
}

// YtdlpGetInfo runs yt-dlp -J -f bestaudio/best URL.
func YtdlpGetInfo(ctx context.Context, url string) (*YTDLPInfo, error) {
	// Ensure yt-dlp is installed once
	installOnce.Do(func() {
		// ignore error here; cmd.Run will surface availability issues
		// If you prefer hard-fail on install error, use MustInstall or check the returned error.
		_ = func() error {
			ytdlp.MustInstall(ctx, nil)
			return nil
		}()
	})

	cmd := ytdlp.New().
		Format("ba[acodec^=opus]/ba[ext=m4a]/bestaudio/best").
		NoCheckCertificates().
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
	ext := infos[0]
	out := &YTDLPInfo{}

	// Playlist/search container
	if len(ext.Entries) > 0 {
		out.Entries = make([]YTDLPEntry, 0, len(ext.Entries))
		for _, e := range ext.Entries {
			if e == nil {
				continue
			}
			entry := YTDLPEntry{
				Id:          e.ID,
				Title:       s(e.Title),
				Uploader:    s(e.Uploader),
				Duration:    f(e.Duration),
				IsLive:      b(e.IsLive),
				Description: s(e.Description),
				WebpageUrl:  s(e.WebpageURL),
				Url:         s(e.URL),
				Thumbnails:  mapThumbs(e.Thumbnails),
				Formats:     mapFormats(e.Formats),
				RequestedFormats: mapReqFormats(
					e.RequestedFormats,
				),
			}
			out.Entries = append(out.Entries, entry)
		}
		// Mirror first non-nil entry to top-level, if any
		for _, first := range ext.Entries {
			if first == nil {
				continue
			}
			out.Id = first.ID
			out.Title = s(first.Title)
			out.Uploader = s(first.Uploader)
			out.Duration = f(first.Duration)
			out.IsLive = b(first.IsLive)
			out.Description = s(first.Description)
			out.WebpageUrl = s(first.WebpageURL)
			out.Url = s(first.URL)
			out.Thumbnails = mapThumbs(first.Thumbnails)
			out.Formats = mapFormats(first.Formats)
			out.RequestedFormats = mapReqFormats(first.RequestedFormats)
			break
		}
		return out, nil
	}

	// Single item
	out.Id = ext.ID
	out.Title = s(ext.Title)
	out.Uploader = s(ext.Uploader)
	out.Duration = f(ext.Duration)
	out.IsLive = b(ext.IsLive)
	out.Description = s(ext.Description)
	out.WebpageUrl = s(ext.WebpageURL)
	out.Url = s(ext.URL)
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
