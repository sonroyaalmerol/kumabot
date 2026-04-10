package player

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/sonroyaalmerol/kumabot/internal/stream"
)

const (
	maxRadioHistory = 50 // Keep track of last 50 songs to avoid repeats
)

// FindRelatedSong finds a song related to the given song using YouTube search.
// It avoids songs with the same title/artist and maintains variety.
func (p *Player) FindRelatedSong(ctx context.Context, current SongMetadata) (*SongMetadata, error) {
	searchQueries := p.buildSearchQueries(current)

	// Try each search query until we find a suitable song
	for _, query := range searchQueries {
		slog.Debug("radio search query", "query", query, "guildID", p.guildID)

		results, err := p.searchYouTube(ctx, query)
		if err != nil {
			slog.Warn("radio search failed", "query", query, "error", err, "guildID", p.guildID)
			continue
		}

		// Find a suitable result that isn't a duplicate
		for _, result := range results {
			if p.isSuitableRadioChoice(result, current) {
				return result, nil
			}
		}
	}

	return nil, fmt.Errorf("could not find a suitable related song")
}

// buildSearchQueries creates a variety of search queries to find related content.
// This mimics YouTube's radio behavior by mixing different strategies.
func (p *Player) buildSearchQueries(current SongMetadata) []string {
	queries := make([]string, 0)

	// Strategy 1: Mix of title keywords and genre/similar artists
	// Extract key terms from title (remove common suffixes like "official video")
	titleKeywords := extractKeywords(current.Title)

	// Strategy 2: Artist + "related" or similar terms
	if current.Artist != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s music", current.Artist),
			fmt.Sprintf("ytsearch5:%s similar artists", current.Artist),
		)
	}

	// Strategy 3: Title keywords with "music" or "song"
	if titleKeywords != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s music", titleKeywords),
			fmt.Sprintf("ytsearch5:%s song", titleKeywords),
		)
	}

	// Strategy 4: Mix title keywords without artist for variety
	if titleKeywords != "" && current.Artist != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s mix", titleKeywords),
			fmt.Sprintf("ytsearch5:similar to %s", titleKeywords),
		)
	}

	// Strategy 5: Genre-based if we can infer it (broad searches for variety)
	queries = append(queries,
		"ytsearch5:music",
		"ytsearch5:popular songs",
	)

	// Shuffle the queries so we don't always try the same order
	rand.Shuffle(len(queries), func(i, j int) {
		queries[i], queries[j] = queries[j], queries[i]
	})

	return queries
}

// searchYouTube performs a YouTube search and returns results as SongMetadata
func (p *Player) searchYouTube(ctx context.Context, query string) ([]*SongMetadata, error) {
	info, err := stream.YtdlpGetInfo(ctx, p.cfg, query)
	if err != nil {
		return nil, err
	}

	var results []*SongMetadata

	// If we got entries from a playlist/search
	if len(info.Entries) > 0 {
		for _, entry := range info.Entries {
			if entry.Id == "" {
				continue
			}
			results = append(results, &SongMetadata{
				Title:     entry.Title,
				Artist:    entry.Uploader,
				VideoID:   entry.Id,
				URL:       entry.Id,
				Length:    int(entry.Duration),
				IsLive:    entry.IsLive,
				Source:    SourceYouTube,
				Thumbnail: extractThumbnail(entry.Thumbnails),
			})
		}
	} else if info.Id != "" {
		// Single result
		results = append(results, &SongMetadata{
			Title:     info.Title,
			Artist:    info.Uploader,
			VideoID:   info.Id,
			URL:       info.Id,
			Length:    int(info.Duration),
			IsLive:    info.IsLive,
			Source:    SourceYouTube,
			Thumbnail: extractThumbnail(info.Thumbnails),
		})
	}

	return results, nil
}

// isSuitableRadioChoice checks if a song is suitable for radio play.
// It avoids:
// 1. Same title/artist as current song
// 2. Songs in the radio history (to avoid repeats)
// 3. Live streams (optional, but usually better for radio)
func (p *Player) isSuitableRadioChoice(candidate *SongMetadata, current SongMetadata) bool {
	// Skip live streams for radio
	if candidate.IsLive {
		return false
	}

	// Normalize strings for comparison
	candTitle := strings.ToLower(strings.TrimSpace(candidate.Title))
	currTitle := strings.ToLower(strings.TrimSpace(current.Title))
	candArtist := strings.ToLower(strings.TrimSpace(candidate.Artist))
	currArtist := strings.ToLower(strings.TrimSpace(current.Artist))

	// Check if same song (same title AND artist)
	if areSimilar(candTitle, currTitle) && areSimilar(candArtist, currArtist) {
		slog.Debug("radio: skipping same song", "title", candidate.Title, "artist", candidate.Artist)
		return false
	}

	// Check history to avoid repeats
	for _, historyID := range p.RadioHistory {
		if candidate.VideoID == historyID {
			slog.Debug("radio: skipping recently played", "title", candidate.Title)
			return false
		}
	}

	return true
}

// addToRadioHistory adds a video ID to the radio history, maintaining max size
func (p *Player) addToRadioHistory(videoID string) {
	p.RadioHistory = append(p.RadioHistory, videoID)
	if len(p.RadioHistory) > maxRadioHistory {
		p.RadioHistory = p.RadioHistory[len(p.RadioHistory)-maxRadioHistory:]
	}
}

// areSimilar checks if two strings are similar (for deduplication)
func areSimilar(a, b string) bool {
	// Direct match
	if a == b {
		return true
	}

	// Check if one contains the other
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}

	// Normalize common variations (remove "official video", "lyrics", etc.)
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "official video", "")
		s = strings.ReplaceAll(s, "official music video", "")
		s = strings.ReplaceAll(s, "lyrics", "")
		s = strings.ReplaceAll(s, "lyric video", "")
		s = strings.ReplaceAll(s, "audio", "")
		s = strings.ReplaceAll(s, "(", "")
		s = strings.ReplaceAll(s, ")", "")
		s = strings.ReplaceAll(s, "[", "")
		s = strings.ReplaceAll(s, "]", "")
		s = strings.TrimSpace(s)
		return s
	}

	return normalize(a) == normalize(b)
}

// extractKeywords extracts key terms from a title, removing common suffixes
func extractKeywords(title string) string {
	// Remove common suffixes and annotations
	clean := strings.ToLower(title)

	// Remove common video type annotations
	removals := []string{
		"official video", "official music video",
		"lyrics", "lyric video",
		"audio", "official audio",
		"hd", "4k", "1080p", "720p",
		"(", ")", "[", "]",
	}

	for _, r := range removals {
		clean = strings.ReplaceAll(clean, r, "")
	}

	// Trim and return first few meaningful words (max 5)
	words := strings.Fields(clean)
	if len(words) > 5 {
		words = words[:5]
	}

	return strings.TrimSpace(strings.Join(words, " "))
}

// extractThumbnail gets the best thumbnail URL from the list
func extractThumbnail(thumbnails []stream.YTDLPThumbnail) string {
	if len(thumbnails) == 0 {
		return ""
	}
	// Return the last one (usually highest quality)
	return thumbnails[len(thumbnails)-1].Url
}
