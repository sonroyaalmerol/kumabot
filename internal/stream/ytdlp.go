package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
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
	// Single item fields
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
func YtdlpGetInfo(ctx context.Context, ytdlpPath, url string) (*YTDLPInfo, error) {
	cmd := exec.CommandContext(ctx, ytdlpPath, "-J", "-f", "bestaudio/best", url)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp run: %w, out: %s", err, out.String())
	}
	var info YTDLPInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp json: %w", err)
	}

	// If it’s a search playlist result, pick first entry into top-level fields.
	if len(info.Entries) > 0 {
		e := info.Entries[0]
		info.Id = e.Id
		info.Title = e.Title
		info.Uploader = e.Uploader
		info.Duration = e.Duration
		info.IsLive = e.IsLive
		info.Description = e.Description
		info.WebpageUrl = e.WebpageUrl
		info.Thumbnails = e.Thumbnails
		info.Formats = e.Formats
		info.RequestedFormats = e.RequestedFormats
		info.Url = e.Url
	}

	return &info, nil
}

// YtdlpAudioURL returns the best playable URL.
// Preferred order: requested_formats (audio/video), top-level url, then formats[].
func YtdlpAudioURL(info *YTDLPInfo) string {
	// requested_formats usually has 2 entries when merging av; pick audio if present
	if len(info.RequestedFormats) > 0 {
		for _, rf := range info.RequestedFormats {
			if strings.HasPrefix(rf.Url, "http") {
				return rf.Url
			}
		}
	}
	// some extractors put direct url at top-level
	if info.Url != "" && strings.HasPrefix(info.Url, "http") {
		return info.Url
	}
	// fallback to formats list
	for _, f := range info.Formats {
		if strings.HasPrefix(f.Url, "http") {
			return f.Url
		}
	}
	// fallback to webpage (ffmpeg can reconnect)
	if info.WebpageUrl != "" {
		return info.WebpageUrl
	}
	return ""
}
