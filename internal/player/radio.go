package player

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"unicode"

	"github.com/sonroyaalmerol/kumabot/internal/stream"
)

const (
	maxRadioHistory           = 50
	maxSameArtistCount        = 3
	titleSimilarityThreshold  = 0.65
	artistSimilarityThreshold = 0.7
)

type radioHistoryEntry struct {
	VideoID string
	Title   string
	Artist  string
}

// ---------- canonical parsing ----------

var (
	reParens        = regexp.MustCompile(`\([^)]*\)`)
	reBrackets      = regexp.MustCompile(`\[[^\]]*\]`)
	reFeat          = regexp.MustCompile(`(?i)\s+(?:feat\.?|ft\.?|featuring)\s+.*`)
	reArtistSep     = regexp.MustCompile(`(?i)\s*(?:,\s*|\s+[&xXvV][sS]?\s+|\s+and\s+)\s*`)
	reDashSep       = regexp.MustCompile(`\s*[-–—]\s*`)
	reYouTubeNoise  = regexp.MustCompile(`(?i)\b(?:official\s+(?:music\s+)?video|music\s+video|official\s+audio|lyric\s+video|lyrics|audio\s+only|full\s+song|hd|4k|1080p|720p|hq|high\s+quality|remastered|remaster|deluxe|explicit|clean\s+version|radio\s+edit|extended|bonus\s+track)\b`)
	reUploaderNoise = regexp.MustCompile(`(?i)\s*[-–—]\s*(?:topic|vevo|official|music|records|recordings|entertainment|tv|net)\s*$`)
	reThePrefix     = regexp.MustCompile(`(?i)^the\s+`)
	reNonAlphaNum   = regexp.MustCompile(`[^a-z0-9\s]`)
	reMultiSpace    = regexp.MustCompile(`\s+`)
)

// canonicalTitleParts extracts all meaningful parts from a raw YouTube title.
// YouTube titles are inconsistent: sometimes "Artist - Title", sometimes "Title - Artist",
// sometimes just "Title". We return both sides of the dash (if any) plus the full cleaned
// title so that isSameTitle can match regardless of order.
//
// Input:  "Adele - Hello (Official Music Video)"
// Output: ["hello", "adele", "adele hello"]
//
// Input:  "Fallen (Live at The Cozy Cove) - Lola Amour"
// Output: ["lola amour", "fallen", "fallen lola amour"]
//
// Input:  "Smells Like Teen Spirit (Remix)"
// Output: ["smells like teen spirit"]
func canonicalTitleParts(raw string) []string {
	s := strings.ToLower(strings.TrimSpace(raw))

	// Strip feat/ft clauses (e.g., "Song Title feat. Other Artist")
	s = reFeat.ReplaceAllString(s, "")

	// Strip parenthetical content: (Official Video), (Remix), (Live), (Deluxe), etc.
	s = reParens.ReplaceAllString(s, "")
	// Strip bracket content: [Official Video], [Lyrics], etc.
	s = reBrackets.ReplaceAllString(s, "")

	// Strip known YouTube noise phrases
	s = reYouTubeNoise.ReplaceAllString(s, "")

	// Split on dash BEFORE clean() — clean() replaces dashes with spaces
	parts := reDashSep.Split(s, 2)
	if len(parts) == 2 {
		left := clean(parts[0])
		right := clean(parts[1])
		if left != "" && right != "" {
			return []string{left, right, left + " " + right}
		}
	}

	return []string{clean(s)}
}

// canonicalTitle extracts the core song name from a raw YouTube title.
// Splits on dash first, then strips noise — preserves original behavior for search queries.
func canonicalTitle(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))

	// Split on dash first, take the part after it (the song name)
	if parts := reDashSep.Split(s, 2); len(parts) == 2 {
		candidate := strings.TrimSpace(parts[1])
		if len(candidate) >= 2 {
			s = candidate
		}
	}

	s = reFeat.ReplaceAllString(s, "")
	s = reParens.ReplaceAllString(s, "")
	s = reBrackets.ReplaceAllString(s, "")
	s = reYouTubeNoise.ReplaceAllString(s, "")

	return clean(s)
}

// canonicalArtist extracts the core artist name from a raw YouTube uploader string.
//
// Input:  "Adele - Topic"
// Output: "adele"
//
// Input:  "QueenOfficial"
// Output: "queenofficial"
//
// Input:  "Artist1 & Artist2"
// Output: ["artist1", "artist2"]
func canonicalArtist(raw string) []string {
	s := strings.ToLower(strings.TrimSpace(raw))

	// Strip YouTube uploader noise: "- Topic", "- VEVO", "Official", etc.
	s = reUploaderNoise.ReplaceAllString(s, "")

	// Strip parenthetical and bracket noise
	s = reParens.ReplaceAllString(s, "")
	s = reBrackets.ReplaceAllString(s, "")

	// Split on collaboration markers: "&", ",", " x ", " and ", "feat."
	// First strip feat clauses
	s = reFeat.ReplaceAllString(s, "")

	parts := reArtistSep.Split(s, -1)
	var artists []string
	for _, p := range parts {
		cleaned := clean(p)
		if cleaned != "" {
			artists = append(artists, cleaned)
		}
	}
	return artists
}

// clean normalizes a string: removes non-alphanumerics, collapses whitespace.
func clean(s string) string {
	s = strings.ToLower(s)
	s = reNonAlphaNum.ReplaceAllString(s, " ")
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// stripThe removes leading "the " from a string for comparison.
func stripThe(s string) string {
	return reThePrefix.ReplaceAllString(s, "")
}

// ---------- similarity metrics ----------

// tokenize splits a string into word tokens.
func tokenize(s string) []string {
	fields := strings.Fields(s)
	seen := make(map[string]bool, len(fields))
	var uniq []string
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	return uniq
}

// jaccardSimilarity computes the Jaccard index of two token sets.
// Returns a value in [0, 1].
func jaccardSimilarity(a, b string) float64 {
	ta := tokenize(a)
	tb := tokenize(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 1.0
	}
	if len(ta) == 0 || len(tb) == 0 {
		return 0.0
	}

	setA := make(map[string]bool, len(ta))
	for _, t := range ta {
		setA[t] = true
	}

	intersection := 0
	for _, t := range tb {
		if setA[t] {
			intersection++
		}
	}

	union := len(ta) + len(tb) - intersection
	if union == 0 {
		return 0.0
	}
	return float64(intersection) / float64(union)
}

// isSameTitle determines if two raw YouTube titles refer to the same song.
// It handles covers, remixes, live versions, lyric videos, etc.
// It also handles inconsistent YouTube title formats where artist/title order varies.
func isSameTitle(rawA, rawB string) bool {
	partsA := canonicalTitleParts(rawA)
	partsB := canonicalTitleParts(rawB)

	// Check all pairwise part combinations for exact or similarity match
	for _, a := range partsA {
		for _, b := range partsB {
			if a == b {
				return true
			}

			// Check containment for short vs long titles
			if len(a) > 0 && len(b) > 0 {
				if strings.Contains(a, b) || strings.Contains(b, a) {
					longer := a
					if len(b) > len(a) {
						longer = b
					}
					shorter := b
					if len(a) < len(b) {
						shorter = a
					}
					if len(tokenize(shorter)) >= 2 && float64(len(shorter))/float64(len(longer)) > 0.5 {
						return true
					}
				}
			}

			if jaccardSimilarity(a, b) >= titleSimilarityThreshold {
				return true
			}
		}
	}

	return false
}

// isSameArtist determines if two raw YouTube uploader names refer to the same artist.
// Handles "The Beatles" vs "Beatles", "Adele - Topic" vs "Adele", collaborations, etc.
func isSameArtist(rawA, rawB string) bool {
	artistsA := canonicalArtist(rawA)
	artistsB := canonicalArtist(rawB)

	for _, a := range artistsA {
		for _, b := range artistsB {
			if artistTokensMatch(a, b) {
				return true
			}
		}
	}
	return false
}

// artistTokensMatch checks if two canonical artist strings match.
func artistTokensMatch(a, b string) bool {
	if a == b {
		return true
	}

	// Handle "the beatles" vs "beatles"
	aNoThe := stripThe(a)
	bNoThe := stripThe(b)
	if aNoThe == bNoThe {
		return true
	}

	// Handle "rolling stones" vs "the rolling stones" — one is subset
	if aNoThe != "" && bNoThe != "" {
		if strings.Contains(aNoThe, bNoThe) || strings.Contains(bNoThe, aNoThe) {
			return true
		}
	}

	return jaccardSimilarity(aNoThe, bNoThe) >= artistSimilarityThreshold
}

// ---------- stop words for title token filtering ----------

var titleStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "in": true,
	"on": true, "at": true, "to": true, "for": true, "and": true,
	"but": true, "or": true, "is": true, "it": true, "by": true,
}

// extractTitleCore gets meaningful tokens from a title for search queries.
func extractTitleCore(raw string) string {
	ct := canonicalTitle(raw)
	tokens := tokenize(ct)
	var filtered []string
	for _, t := range tokens {
		if !titleStopWords[t] {
			filtered = append(filtered, t)
		}
	}
	return strings.Join(filtered, " ")
}

// ---------- main radio logic ----------

// FindRelatedSong finds a song related to the given song using YouTube search.
func (p *Player) FindRelatedSong(ctx context.Context, current SongMetadata) (*SongMetadata, error) {
	searchQueries := p.buildSearchQueries(current)

	for _, query := range searchQueries {
		slog.Debug("radio search query", "query", query, "guildID", p.guildID)

		results, err := p.searchYouTube(ctx, query)
		if err != nil {
			slog.Warn("radio search failed", "query", query, "error", err, "guildID", p.guildID)
			continue
		}

		for _, result := range results {
			if p.isSuitableRadioChoice(result, current) {
				return result, nil
			}
		}
	}

	return nil, fmt.Errorf("could not find a suitable related song")
}

// buildSearchQueries creates varied search queries for related content.
func (p *Player) buildSearchQueries(current SongMetadata) []string {
	queries := make([]string, 0)

	titleCore := extractTitleCore(current.Title)
	artists := canonicalArtist(current.Artist)
	artistStr := ""
	if len(artists) > 0 {
		artistStr = artists[0]
	}

	// Strategy 1: Artist + genre-like queries for discovery
	if artistStr != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s similar songs", artistStr),
			fmt.Sprintf("ytsearch5:songs like %s", artistStr),
		)
	}

	// Strategy 2: Title keywords for genre/mood matching
	if titleCore != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:songs like %s", titleCore),
		)
	}

	// Strategy 3: Artist + title mix for very close matches
	if artistStr != "" && titleCore != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s %s", artistStr, titleCore),
		)
	}

	// Strategy 4: Second artist from collaboration
	if len(artists) > 1 {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s music", artists[1]),
		)
	}

	// Strategy 5: Broad discovery
	if artistStr != "" {
		queries = append(queries,
			fmt.Sprintf("ytsearch5:%s", artistStr),
		)
	}

	// Shuffle for variety
	rand.Shuffle(len(queries), func(i, j int) {
		queries[i], queries[j] = queries[j], queries[i]
	})

	return queries
}

// searchYouTube performs a YouTube search and returns results as SongMetadata.
func (p *Player) searchYouTube(ctx context.Context, query string) ([]*SongMetadata, error) {
	info, err := stream.YtdlpGetInfo(ctx, p.cfg, query)
	if err != nil {
		return nil, err
	}

	var results []*SongMetadata

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
func (p *Player) isSuitableRadioChoice(candidate *SongMetadata, current SongMetadata) bool {
	if candidate.IsLive {
		return false
	}

	// Reject same/similar title regardless of artist (catches covers, remixes, lyric videos)
	if isSameTitle(candidate.Title, current.Title) {
		slog.Debug("radio: skipping similar title",
			"candidate", candidate.Title,
			"current", current.Title,
			"guildID", p.guildID)
		return false
	}

	// Reject exact video ID repeats from history
	for _, entry := range p.RadioHistory {
		if candidate.VideoID == entry.VideoID {
			slog.Debug("radio: skipping recently played", "title", candidate.Title, "guildID", p.guildID)
			return false
		}
	}

	// Reject if same title appeared in history (covers of previously played songs)
	for _, entry := range p.RadioHistory {
		if isSameTitle(candidate.Title, entry.Title) {
			slog.Debug("radio: skipping title from history",
				"candidate", candidate.Title,
				"guildID", p.guildID)
			return false
		}
	}

	// Cap same-artist songs in history to prevent discography runs
	sameArtistCount := 0
	for _, entry := range p.RadioHistory {
		if isSameArtist(candidate.Artist, entry.Artist) {
			sameArtistCount++
		}
	}

	if sameArtistCount >= maxSameArtistCount {
		slog.Debug("radio: skipping artist cap reached",
			"artist", candidate.Artist,
			"count", sameArtistCount,
			"guildID", p.guildID)
		return false
	}

	return true
}

func (p *Player) addToRadioHistory(videoID string, title string, artist string) {
	p.RadioHistory = append(p.RadioHistory, radioHistoryEntry{VideoID: videoID, Title: title, Artist: artist})
	if len(p.RadioHistory) > maxRadioHistory {
		p.RadioHistory = p.RadioHistory[len(p.RadioHistory)-maxRadioHistory:]
	}
}

func extractThumbnail(thumbnails []stream.YTDLPThumbnail) string {
	if len(thumbnails) == 0 {
		return ""
	}
	return thumbnails[len(thumbnails)-1].Url
}

// isSeparator reports whether a rune is a dash-like separator used in YouTube titles.
func isSeparator(r rune) bool {
	return r == '-' || r == '–' || r == '—' || r == '|' || r == '/'
}

// normalizeRunes lowercases and strips diacritics-like noise for token comparison.
func normalizeRunes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else if unicode.IsSpace(r) || isSeparator(r) {
			b.WriteRune(' ')
		}
	}
	return b.String()
}
