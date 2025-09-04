package stream

import (
	"context"
	"fmt"
	"os"
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

type MediaURL struct {
	Kind string // "direct" or "hls"
	URL  string
}

func PickMediaURL(info *YTDLPInfo) MediaURL {
	// 1) prefer direct (requested -> top-level -> formats)
	for _, rf := range info.RequestedFormats {
		u := rf.Url
		if strings.HasPrefix(u, "http") && !isManifestURL(u) && isLikelyMediaURL(u) {
			ytdlpDebugf("PickMediaURL: DIRECT requested %s", u)
			return MediaURL{Kind: "direct", URL: u}
		}
	}
	if strings.HasPrefix(info.Url, "http") && !isManifestURL(info.Url) && isLikelyMediaURL(info.Url) {
		ytdlpDebugf("PickMediaURL: DIRECT top-level %s", info.Url)
		return MediaURL{Kind: "direct", URL: info.Url}
	}
	for _, f := range info.Formats {
		u := f.Url
		if strings.HasPrefix(u, "http") && !isManifestURL(u) && isLikelyMediaURL(u) {
			ytdlpDebugf("PickMediaURL: DIRECT formats %s", u)
			return MediaURL{Kind: "direct", URL: u}
		}
	}
	// 2) HLS fallback (requested -> top-level -> formats)
	for _, rf := range info.RequestedFormats {
		u := rf.Url
		if strings.HasPrefix(u, "http") && isManifestURL(u) {
			ytdlpDebugf("PickMediaURL: HLS requested %s", u)
			return MediaURL{Kind: "hls", URL: u}
		}
	}
	if strings.HasPrefix(info.Url, "http") && isManifestURL(info.Url) {
		ytdlpDebugf("PickMediaURL: HLS top-level %s", info.Url)
		return MediaURL{Kind: "hls", URL: info.Url}
	}
	for _, f := range info.Formats {
		u := f.Url
		if strings.HasPrefix(u, "http") && isManifestURL(u) {
			ytdlpDebugf("PickMediaURL: HLS formats %s", u)
			return MediaURL{Kind: "hls", URL: u}
		}
	}
	ytdlpDebugf("PickMediaURL: none")
	return MediaURL{}
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

func ytdlpDebugf(format string, args ...any) {
	if debugOn() {
		_, _ = fmt.Fprintf(os.Stderr, "[stream/ytdlp] "+format+"\n", args...)
	}
}

// YtdlpGetInfo runs yt-dlp -J -f bestaudio/best URL.
func YtdlpGetInfo(ctx context.Context, url string) (*YTDLPInfo, error) {
	installOnce.Do(func() { _ = func() error { ytdlp.MustInstall(ctx, nil); return nil }() })

	cmd := ytdlp.New().
		Format("ba[acodec^=opus]/ba[ext=m4a]/bestaudio/best").
		NoCheckCertificates().
		DumpJSON()

	ytdlpDebugf("running yt-dlp for URL: %s", url)
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
		ytdlpDebugf("got playlist with %d entries", len(ext.Entries))
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

	ytdlpDebugf("single item: id=%s title=%s is_live=%v url=%s", out.Id, out.Title, out.IsLive, out.Url)

	return out, nil
}

func isManifestURL(u string) bool {
	if u == "" {
		return false
	}
	us := strings.ToLower(u)
	return strings.Contains(us, ".m3u8") ||
		strings.Contains(us, ".mpd") ||
		strings.Contains(us, "application%2fx-mpegurl") ||
		strings.Contains(us, "application/x-mpegurl") ||
		strings.Contains(us, "application%2fdash+xml") ||
		strings.Contains(us, "application/dash+xml")
}

func isLikelyMediaURL(u string) bool {
	if u == "" || !strings.HasPrefix(u, "http") {
		return false
	}
	lu := strings.ToLower(u)
	// Reject known thumbnail/static domains and extensions
	if strings.Contains(lu, "i.ytimg.com") ||
		strings.Contains(lu, "googleusercontent.com") {
		return false
	}
	// Favor typical audio media hints
	if strings.Contains(lu, ".webm") || strings.Contains(lu, "audio/webm") ||
		strings.Contains(lu, ".m4a") || strings.Contains(lu, "audio/mp4") ||
		strings.Contains(lu, "opus") || strings.Contains(lu, "aac") ||
		strings.Contains(lu, "mp4a") {
		return true
	}
	// Some direct audio URLs lack extensions; accept as fallback if not manifest
	return !isManifestURL(lu)
}
