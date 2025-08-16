package player

import (
	"regexp"
	"sort"
	"strings"
)

// parseChaptersFromDescription extracts chapter markers (label + timestamp) from the description.
// It returns a sorted list by offset and computes segment lengths.
func parseChaptersFromDescription(description string, videoDurationSeconds int) []struct {
	Label  string
	Offset int
	Length int
} {
	lines := strings.Split(description, "\n")
	tsRe := regexp.MustCompile(`(?:\d+:)+\d+`) // matches 0:00, 12:34, 1:23:45 etc.

	type rawChapter struct {
		label  string
		offset int
	}
	var found []rawChapter
	foundFirstAtZero := false

	for _, line := range lines {
		matches := tsRe.FindAllString(line, -1)
		if len(matches) != 1 {
			continue
		}
		timestamp := matches[0]
		secs := parseTS(timestamp)
		if !foundFirstAtZero {
			// The first must be 0:00 or 00:00 to consider it's a true chapter list,
			if secs == 0 {
				foundFirstAtZero = true
			} else {
				continue
			}
		}
		label := strings.TrimSpace(strings.TrimPrefix(line, timestamp))
		if label == "" {
			// sometimes it's "0:00 Intro" or "0:00 - Intro"
			// try removing separators:
			// split by timestamp and take tail
			parts := strings.Split(line, timestamp)
			if len(parts) > 1 {
				label = strings.TrimSpace(parts[1])
				label = strings.TrimLeft(label, "-:–—|> ")
			}
		}
		if label == "" {
			label = "Chapter"
		}
		found = append(found, rawChapter{label: label, offset: secs})
	}

	if len(found) == 0 || !foundFirstAtZero {
		return nil
	}

	// Sort by offset and compute lengths
	sort.Slice(found, func(i, j int) bool { return found[i].offset < found[j].offset })

	out := make([]struct {
		Label  string
		Offset int
		Length int
	}, 0, len(found))

	for i := 0; i < len(found); i++ {
		start := found[i].offset
		var end int
		if i == len(found)-1 {
			end = videoDurationSeconds
		} else {
			end = found[i+1].offset
		}
		if end > start && start >= 0 {
			out = append(out, struct {
				Label  string
				Offset int
				Length int
			}{
				Label:  found[i].label,
				Offset: start,
				Length: end - start,
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseTS(s string) int {
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		total = total*60 + atoiSafe(strings.TrimSpace(p))
	}
	return total
}

func atoiSafe(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
