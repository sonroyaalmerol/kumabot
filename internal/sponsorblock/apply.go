package sponsorblock

import (
	"context"
	"strings"
	"time"
)

type Applier struct {
	client        *Client
	cache         *Cache[[]Segment]
	disabledUntil time.Time
	disableFor    time.Duration
}

func NewApplier(timeoutMinutes int) *Applier {
	return &Applier{
		client:     NewClient(),
		cache:      NewCache[[]Segment](time.Hour),
		disableFor: time.Duration(timeoutMinutes) * time.Minute,
	}
}

// Adjust modifies offset and length based on music_offtopic segments.
// Returns a message (e.g., "skipped intro, trimmed outro") if changes occurred.
func (a *Applier) Adjust(ctx context.Context, youtubeID string, lengthSec int, offsetSec int) (newLen int, newOff int, msg string, changed bool) {
	if youtubeID == "" || lengthSec <= 0 {
		return lengthSec, offsetSec, "", false
	}
	if time.Now().Before(a.disabledUntil) {
		return lengthSec, offsetSec, "", false
	}

	key := "music_offtopic:" + youtubeID
	segs, ok := a.cache.Get(key)
	if !ok {
		var err error
		segs, err = a.client.GetSegments(ctx, youtubeID, []string{"music_offtopic"})
		if err != nil {
			if err.Error() == "sb_504" {
				a.disabledUntil = time.Now().Add(a.disableFor)
			}
			return lengthSec, offsetSec, "", false
		}
		a.cache.Set(key, segs)
	}
	if len(segs) == 0 {
		return lengthSec, offsetSec, "", false
	}
	segs = MergeSegments(segs)

	newLen, newOff, changed = lengthSec, offsetSec, false
	msgParts := []string{}

	// Outro: if last segment ends within last ~2 seconds, trim tail
	last := segs[len(segs)-1]
	if last.Segment[1] >= float64(lengthSec-2) {
		trim := int(last.Segment[1])
		if trim < lengthSec {
			newLen = trim
			changed = true
			msgParts = append(msgParts, "trimmed outro")
		}
	}

	// Intro: if first segment starts near zero (<= 2s), skip intro
	first := segs[0]
	if first.Segment[0] <= 2.0 {
		skip := int(first.Segment[1])
		if skip > 0 && skip < newLen {
			newOff += skip
			newLen -= skip
			changed = true
			msgParts = append(msgParts, "skipped intro")
		}
	}

	if changed {
		msg = strings.Join(msgParts, ", ")
	}
	return newLen, newOff, msg, changed
}
