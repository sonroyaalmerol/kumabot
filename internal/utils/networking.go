package utils

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
)

func RandomUserAgent() string {
	// Target Chrome major versions roughly within last ~6 months
	const minMajor = 132
	const maxMajor = 138

	major := rand.IntN(maxMajor-minMajor+1) + minMajor
	return fmt.Sprintf(
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36",
		major,
	)
}

// BuildFFmpegHeaders builds a CRLF-joined header string for AVFormat "headers" option.
// It filters/normalizes a map of headers and returns "Key: Value\r\n..." content.
func BuildFFmpegHeaders(base map[string]string) string {
	if len(base) == 0 {
		return ""
	}
	// Normalize keys to canonical-case commonly accepted by servers
	canon := func(k string) string {
		k = strings.TrimSpace(k)
		low := strings.ToLower(k)
		switch low {
		case "user-agent":
			return "User-Agent"
		case "referer":
			return "Referer"
		case "accept":
			return "Accept"
		case "accept-language":
			return "Accept-Language"
		case "origin":
			return "Origin"
		case "connection":
			return "Connection"
		case "cookie":
			return "Cookie"
		case "range":
			return "Range"
		case "authorization":
			return "Authorization"
		default:
			// Keep as-is with first-letter uppercase heuristic
			if len(k) == 0 {
				return k
			}
			return strings.ToUpper(k[:1]) + k[1:]
		}
	}

	// Copy and sanitize
	h := maps.Clone(base)
	// Ensure required defaults if missing
	if _, ok := h["User-Agent"]; !ok {
		h["User-Agent"] = RandomUserAgent()
	}
	if _, ok := h["Referer"]; !ok {
		h["Referer"] = "https://www.youtube.com/"
	}
	if _, ok := h["Accept"]; !ok {
		h["Accept"] = "*/*"
	}
	if _, ok := h["Accept-Language"]; !ok {
		h["Accept-Language"] = "en-US,en;q=0.9"
	}
	if _, ok := h["Origin"]; !ok {
		h["Origin"] = "https://www.youtube.com"
	}
	if _, ok := h["Connection"]; !ok {
		h["Connection"] = "keep-alive"
	}

	// Order headers (stable, human-friendly)
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, canon(k))
	}
	// Remove dups after canonicalization
	keys = slices.CompactFunc(keys, func(a, b string) bool { return strings.EqualFold(a, b) })
	slices.SortFunc(keys, strings.Compare)

	var b strings.Builder
	for _, k := range keys {
		v := h[k]
		// FFmpeg wants CRLF separators; no trailing extra CRLF needed
		fmt.Fprintf(&b, "%s: %s\r\n", k, strings.TrimSpace(v))
	}
	return b.String()
}
