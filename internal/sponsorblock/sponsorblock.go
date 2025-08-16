package sponsorblock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const base = "https://sponsor.ajay.app/api/skipSegments"

type Segment struct {
	Category   string     `json:"category"`
	Segment    [2]float64 `json:"segment"` // [start, end] seconds
	UUID       string     `json:"UUID"`
	ActionType string     `json:"actionType"`
}

type Client struct {
	http  *http.Client
	sleep func(time.Duration)
}

func NewClient() *Client {
	return &Client{
		http:  &http.Client{Timeout: 8 * time.Second},
		sleep: time.Sleep,
	}
}

// GetSegments fetches segments of specified categories for the given YouTube video ID.
func (c *Client) GetSegments(ctx context.Context, videoID string, categories []string) ([]Segment, error) {
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("videoID", videoID)
	for _, cat := range categories {
		q.Add("categories", cat)
	}
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// 404 means no segments for this video
		return []Segment{}, nil
	}
	if resp.StatusCode == 504 {
		return nil, fmt.Errorf("sb_504")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sb_http_%d", resp.StatusCode)
	}
	var segs []Segment
	if err := json.NewDecoder(resp.Body).Decode(&segs); err != nil {
		return nil, err
	}
	return segs, nil
}

// Merge overlapping segments (assumes same category list).
func MergeSegments(segs []Segment) []Segment {
	if len(segs) == 0 {
		return segs
	}
	// sort by start
	sort.Slice(segs, func(i, j int) bool {
		return segs[i].Segment[0] < segs[j].Segment[0]
	})
	out := []Segment{segs[0]}
	for _, s := range segs[1:] {
		last := &out[len(out)-1]
		if s.Segment[0] <= last.Segment[1] {
			// overlap -> extend
			if s.Segment[1] > last.Segment[1] {
				last.Segment[1] = s.Segment[1]
			}
		} else {
			out = append(out, s)
		}
	}
	return out
}
