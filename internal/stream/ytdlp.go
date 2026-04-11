package stream

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	ytdlp "github.com/lrstanley/go-ytdlp"
	"github.com/sonroyaalmerol/kumabot/internal/config"
)

type YTDLPRequestedFormat struct {
	Url string `json:"url"`
}

type YTDLPFormat struct {
	Url string `json:"url"`
}

type YTDLPThumbnail struct {
	Url string `json:"url"`
}

type YTDLPEntry struct {
	Id               string                 `json:"id"`
	Title            string                 `json:"title"`
	Uploader         string                 `json:"uploader"`
	Duration         float64                `json:"duration"`
	IsLive           bool                   `json:"is_live"`
	Description      string                 `json:"description"`
	WebpageUrl       string                 `json:"webpage_url"`
	Thumbnails       []YTDLPThumbnail       `json:"thumbnails"`
	Formats          []YTDLPFormat          `json:"formats"`
	RequestedFormats []YTDLPRequestedFormat `json:"requested_formats"`
	Url              string                 `json:"url"`
}

type YTDLPInfo struct {
	Id               string                 `json:"id"`
	Title            string                 `json:"title"`
	Uploader         string                 `json:"uploader"`
	Duration         float64                `json:"duration"`
	IsLive           bool                   `json:"is_live"`
	Description      string                 `json:"description"`
	WebpageUrl       string                 `json:"webpage_url"`
	Thumbnails       []YTDLPThumbnail       `json:"thumbnails"`
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

func mapThumbs(ts []*ytdlp.ExtractedThumbnail) []YTDLPThumbnail {
	if len(ts) == 0 {
		return nil
	}
	out := make([]YTDLPThumbnail, 0, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		out = append(out, YTDLPThumbnail{Url: t.URL})
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
func YtdlpGetInfo(ctx context.Context, cfg *config.Config, url string) (*YTDLPInfo, error) {
	installOnce.Do(func() { _ = func() error { ytdlp.MustInstall(ctx, nil); return nil }() })

	cmd := ytdlp.New().
		Format("ba[acodec^=opus]/ba[ext=m4a]/bestaudio/best").
		NoCheckCertificates().
		DumpJSON()

	if cfg.YouTubeCookiesPath != "" {
		cmd = cmd.Cookies(cfg.YouTubeCookiesPath)
		ytdlpDebugf("using cookies from file: %s", cfg.YouTubeCookiesPath)
	}

	if strings.Contains(url, "youtube.com") || strings.Contains(url, "youtu.be") {
		extractorArgs := "youtube:player-client=default,mweb"
		if cfg.YouTubePOToken != "" {
			extractorArgs += ";po_token=" + cfg.YouTubePOToken
		}
		cmd = cmd.ExtractorArgs(extractorArgs)
		ytdlpDebugf("using YouTube extractor args: player-client=default,mweb")
	}

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

	mapExtToEntry := func(e *ytdlp.ExtractedInfo) YTDLPEntry {
		return YTDLPEntry{
			Id:               e.ID,
			Title:            s(e.Title),
			Uploader:         s(e.Uploader),
			Duration:         f(e.Duration),
			IsLive:           b(e.IsLive),
			Description:      s(e.Description),
			WebpageUrl:       s(e.WebpageURL),
			Url:              s(e.URL),
			Thumbnails:       mapThumbs(e.Thumbnails),
			Formats:          mapFormats(e.Formats),
			RequestedFormats: mapReqFormats(e.RequestedFormats),
		}
	}

	mapExtToInfo := func(e *ytdlp.ExtractedInfo) {
		out.Id = e.ID
		out.Title = s(e.Title)
		out.Uploader = s(e.Uploader)
		out.Duration = f(e.Duration)
		out.IsLive = b(e.IsLive)
		out.Description = s(e.Description)
		out.WebpageUrl = s(e.WebpageURL)
		out.Url = s(e.URL)
		out.Thumbnails = mapThumbs(e.Thumbnails)
		out.Formats = mapFormats(e.Formats)
		out.RequestedFormats = mapReqFormats(e.RequestedFormats)
	}

	// Playlist container with nested entries
	if len(ext.Entries) > 0 {
		ytdlpDebugf("got playlist with %d entries", len(ext.Entries))
		out.Entries = make([]YTDLPEntry, 0, len(ext.Entries))
		for _, e := range ext.Entries {
			if e == nil {
				continue
			}
			out.Entries = append(out.Entries, mapExtToEntry(e))
		}
		for _, first := range ext.Entries {
			if first == nil {
				continue
			}
			mapExtToInfo(first)
			break
		}
		return out, nil
	}

	// Multiple top-level items (e.g. ytsearch10: queries return each result separately)
	if len(infos) > 1 {
		ytdlpDebugf("got %d top-level items (search results)", len(infos))
		out.Entries = make([]YTDLPEntry, 0, len(infos))
		for _, item := range infos {
			if item == nil {
				continue
			}
			out.Entries = append(out.Entries, mapExtToEntry(item))
		}
		if len(out.Entries) > 0 {
			first := infos[0]
			mapExtToInfo(first)
		}
		return out, nil
	}

	// Single item
	mapExtToInfo(ext)

	ytdlpDebugf("single item: id=%s title=%s is_live=%v url=%s", out.Id, out.Title, out.IsLive, out.Url)

	return out, nil
}

// YtdlpGetRelated fetches YouTube's auto-generated "Radio" mix for a video using the RD playlist.
// Returns up to limit related videos as flat entries.
func YtdlpGetRelated(ctx context.Context, cfg *config.Config, videoID string, limit int) ([]YTDLPEntry, error) {
	installOnce.Do(func() { _ = func() error { ytdlp.MustInstall(ctx, nil); return nil }() })

	url := fmt.Sprintf("https://www.youtube.com/watch?v=%s&list=RD%s", videoID, videoID)

	cmd := ytdlp.New().
		FlatPlaylist().
		DumpJSON().
		PlaylistEnd(limit)

	if cfg.YouTubeCookiesPath != "" {
		cmd = cmd.Cookies(cfg.YouTubeCookiesPath)
	}

	if cfg.YouTubePOToken != "" {
		cmd = cmd.ExtractorArgs("youtube:player-client=default,mweb;po_token=" + cfg.YouTubePOToken)
	}

	ytdlpDebugf("running yt-dlp for related: %s (limit=%d)", url, limit)
	res, err := cmd.Run(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("yt-dlp related: %w", err)
	}

	infos, err := res.GetExtractedInfo()
	if err != nil {
		return nil, fmt.Errorf("parse yt-dlp related json: %w", err)
	}

	var entries []YTDLPEntry
	for _, item := range infos {
		if item == nil || item.ID == "" {
			continue
		}
		entries = append(entries, YTDLPEntry{
			Id:        item.ID,
			Title:     s(item.Title),
			Uploader:  s(item.Uploader),
			Duration:  f(item.Duration),
			IsLive:    b(item.IsLive),
		})
		if len(entries) >= limit {
			break
		}
	}

	ytdlpDebugf("got %d related entries for %s", len(entries), videoID)
	return entries, nil
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

const (
	DefaultInfoTimeout    = 30 * time.Second
	DefaultRelatedTimeout = 45 * time.Second
)

// YtdlpGetInfoWithTimeout calls YtdlpGetInfo with a timeout and one retry on failure.
func YtdlpGetInfoWithTimeout(ctx context.Context, cfg *config.Config, url string, timeout time.Duration) (*YTDLPInfo, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, timeout)
	defer cancel1()
	info, err := YtdlpGetInfo(ctx1, cfg, url)
	if err == nil {
		return info, nil
	}
	ytdlpDebugf("yt-dlp info attempt 1 failed for %s: %v, retrying...", url, err)
	ctx2, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()
	return YtdlpGetInfo(ctx2, cfg, url)
}

// YtdlpGetRelatedWithTimeout calls YtdlpGetRelated with a timeout and one retry on failure.
func YtdlpGetRelatedWithTimeout(ctx context.Context, cfg *config.Config, videoID string, limit int, timeout time.Duration) ([]YTDLPEntry, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, timeout)
	defer cancel1()
	entries, err := YtdlpGetRelated(ctx1, cfg, videoID, limit)
	if err == nil {
		return entries, nil
	}
	ytdlpDebugf("yt-dlp related attempt 1 failed for %s: %v, retrying...", videoID, err)
	ctx2, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()
	return YtdlpGetRelated(ctx2, cfg, videoID, limit)
}
