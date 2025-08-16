package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

type YTDLPFormat struct {
	Url string `json:"url"`
}

type YTDLPInfo struct {
	Id          string `json:"id"`
	Title       string `json:"title"`
	Uploader    string `json:"uploader"`
	Duration    int    `json:"duration"`
	IsLive      bool   `json:"is_live"`
	Description string `json:"description"`
	Thumbnails  []struct {
		Url string `json:"url"`
	} `json:"thumbnails"`
	Formats    []YTDLPFormat `json:"formats"`
	WebpageUrl string        `json:"webpage_url"`
}

func YtdlpGetInfo(ctx context.Context, ytdlpPath, url string) (*YTDLPInfo, error) {
	cmd := exec.CommandContext(ctx, ytdlpPath, "-J", "-f", "bestaudio/best", url)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var info YTDLPInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func YtdlpAudioURL(info *YTDLPInfo) string {
	// yt-dlp -J already resolves direct URL in "url" field
	// If missing, fallback to webpage_url for ffmpeg with -reconnect
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
