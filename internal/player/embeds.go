package player

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/utils"
)

func songLink(s SongMetadata) string {
	if s.Source == SourceHLS {
		return fmt.Sprintf("[%s](%s)", s.Title, s.URL)
	}
	yid := s.VideoID
	if len(yid) != 11 {
		// If URL stored, extract id as needed or just link raw URL
		return fmt.Sprintf("[%s](%s)", s.Title, "https://www.youtube.com/watch?v="+yid)
	}
	link := "https://www.youtube.com/watch?v=" + yid
	if s.Offset > 0 {
		link += "&t=" + fmt.Sprint(s.Offset)
	}
	return fmt.Sprintf("[%s](%s)", s.Title, link)
}

// Brand colors — single source of truth for embed accent colors.
const (
	ColorPlaying = 0x2ECC71 // Bright green — active playback
	ColorPaused  = 0xE74C3C // Red — paused / idle
	ColorQueue   = 0x2ECC71 // Matches playing state; changes in BuildQueueEmbed
	ColorIdle    = 0x95A5A6 // Muted grey — nothing playing
	ColorBrand   = 0x1DB954 // Kuma brand green (Spotify-ish)

	// Wrapped slide colors — derived from brand palette
	ColorWrappedHeader   = 0x1DB954
	ColorWrappedSongs    = 0x2ECC71
	ColorWrappedArtists  = 0x9B59B6
	ColorWrappedDJs      = 0x3498DB
	ColorWrappedHabits   = 0xE74C3C
	ColorWrappedActivity = 0x1ABC9C
)

func BuildPlayingEmbed(p *Player) *discordgo.MessageEmbed {
	const maxDesc = 4096

	cur := p.GetCurrent()
	if cur == nil {
		return &discordgo.MessageEmbed{
			Title:       "Nothing Playing",
			Description: "No playing song found",
			Color:       ColorIdle,
		}
	}

	p.mu.Lock()
	pos := p.GetPosition()
	status := p.StatusPub()
	loopSong := p.LoopSongPub()
	loopQueue := p.LoopQueuePub()
	shuffle := p.ShuffleModePub()
	radio := p.IsRadioMode()
	searching := p.IsSearching()
	queue := p.SongQueue
	qpos := p.Qpos
	p.mu.Unlock()

	// --- Now playing section ---
	progress := 0.0
	if cur.Length > 0 {
		progress = float64(pos) / float64(cur.Length)
	}
	bar := ProgressBar(12, progress)
	elapsed := "LIVE"
	if !cur.IsLive {
		elapsed = fmt.Sprintf("%s / %s", utils.PrettyTime(pos), utils.PrettyTime(cur.Length))
	}

	var tags []string
	if loopSong {
		tags = append(tags, "🔂 Song")
	} else if loopQueue {
		tags = append(tags, "🔁 Queue")
	}
	if shuffle {
		tags = append(tags, "🔀 Shuffle")
	}
	if radio {
		tags = append(tags, "📻 Radio")
	}

	desc := fmt.Sprintf("**%s**\n<@%s>\n%s `[ %s ]`",
		songLink(*cur),
		cur.RequestedBy,
		bar, elapsed,
	)
	if len(tags) > 0 {
		desc += "  " + strings.Join(tags, " ")
	}

	// --- Queue section ---
	upcoming := queue[qpos+1:]
	if len(upcoming) > 0 || searching {
		desc += "\n\n**Up Next:**\n"

		// Budget remaining chars for queue lines
		remaining := maxDesc - len(desc) - 30 // reserve 30 for "…and N more"

		for i, s := range upcoming {
			dur := "LIVE"
			if !s.IsLive {
				dur = utils.PrettyTime(s.Length)
			}
			prefix := ""
			if s.IsRadioSuggestion {
				prefix = "📻 "
			}
			line := fmt.Sprintf("`%d.` %s%s `[ %s ]`\n", i+1, prefix, songLink(s), dur)
			if len(line) > remaining {
				// Can't fit this line — show count of remaining
				extra := len(upcoming) - i
				more := fmt.Sprintf("…and **%d** more", extra)
				if len(desc)+len(more)+1 <= maxDesc {
					desc += more
				}
				break
			}
			desc += line
			remaining -= len(line)
		}

		if searching && len(desc)+50 <= maxDesc {
			desc += "\n*Still resolving…*"
		}
	}

	color := ColorPlaying
	title := "Now Playing"
	if status != StatusPlaying {
		color = ColorPaused
		title = "Paused"
	}

	footer := "kumabot"
	if cur.Artist != "" {
		footer = fmt.Sprintf("Source: %s • kumabot", cur.Artist)
		if cur.Playlist != nil {
			footer = fmt.Sprintf("Source: %s (%s) • kumabot", cur.Artist, cur.Playlist.Title)
		}
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: desc,
		Color:       color,
		Footer: &discordgo.MessageEmbedFooter{
			Text: footer,
		},
	}
	if cur.Thumbnail != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: cur.Thumbnail}
	}
	return embed
}

// PlayingComponents returns the playback control button rows for the now-playing embed.
func PlayingComponents(p *Player) []discordgo.MessageComponent {
	pauseLabel := "⏸"
	pauseStyle := discordgo.SecondaryButton
	if p.StatusPub() != StatusPlaying {
		pauseLabel = "▶"
		pauseStyle = discordgo.SuccessButton
	}

	loopStyle := discordgo.SecondaryButton
	if p.LoopSongPub() {
		loopStyle = discordgo.SuccessButton
	}
	radioStyle := discordgo.SecondaryButton
	if p.IsRadioMode() {
		radioStyle = discordgo.SuccessButton
	}
	shuffleStyle := discordgo.SecondaryButton
	if p.ShuffleModePub() {
		shuffleStyle = discordgo.SuccessButton
	}

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "⏮", Style: discordgo.SecondaryButton, CustomID: "kuma:prev"},
				discordgo.Button{Label: pauseLabel, Style: pauseStyle, CustomID: "kuma:pause"},
				discordgo.Button{Label: "⏭", Style: discordgo.SecondaryButton, CustomID: "kuma:next"},
			},
		},
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "🔁", Style: loopStyle, CustomID: "kuma:loop"},
				discordgo.Button{Label: "🔀", Style: shuffleStyle, CustomID: "kuma:shuffle"},
				discordgo.Button{Label: "📻", Style: radioStyle, CustomID: "kuma:radio"},
				discordgo.Button{Label: "⏹", Style: discordgo.DangerButton, CustomID: "kuma:stop"},
			},
		},
	}
}

func BuildQueueEmbed(
	p *Player,
	page int,
	pageSize int,
) (*discordgo.MessageEmbed, error) {
	const maxDesc = 4096

	cur := p.GetCurrent()
	if cur == nil {
		return nil, fmt.Errorf("queue is empty")
	}

	status := p.StatusPub()
	color := ColorPlaying
	if status != StatusPlaying {
		color = ColorPaused
	}
	total := p.QueueSize()
	maxPage := (total + 1 + pageSize - 1) / pageSize
	if page > maxPage {
		return nil, fmt.Errorf("the queue isn't that big")
	}
	items, _ := p.GetQueuePage(page, pageSize)

	// Build list
	var out strings.Builder
	begin := (page - 1) * pageSize
	for idx, s := range items {
		n := begin + idx + 1
		dur := "live"
		if !s.IsLive {
			dur = utils.PrettyTime(s.Length)
		}
		out.WriteString(fmt.Sprintf("`%d.` %s `[ %s ]`\n", n, songLink(s), dur))
	}

	totalLen := 0
	p.mu.Lock()
	for _, s := range p.SongQueue[p.Qpos+1:] {
		totalLen += s.Length
	}
	p.mu.Unlock()

	desc := fmt.Sprintf(
		"**%s**\nRequested by: <@%s>\n\n",
		songLink(*cur),
		cur.RequestedBy,
	)

	// Now-playing UI line
	pos := p.GetPosition()
	progress := 0.0
	if cur.Length > 0 {
		progress = float64(pos) / float64(cur.Length)
	}
	bar := ProgressBar(10, progress)
	elapsed := "live"
	if !cur.IsLive {
		elapsed = fmt.Sprintf(
			"%s/%s",
			utils.PrettyTime(pos),
			utils.PrettyTime(cur.Length),
		)
	}
	loop := ""
	if p.LoopSongPub() {
		loop = " (loop on)"
	}
	desc += fmt.Sprintf("%s `[ %s ]`\n\n", bar, elapsed)

	// Show resolving note inline in the description so users don’t miss it
	if p.IsSearching() {
		desc += "Note: still resolving more items… they will appear here as they’re ready.\n\n"
	}

	if len(items) > 0 {
		// Add header first
		upNextHeader := "**Up next:**\n"
		desc += upNextHeader

		// Budget remaining for the queue lines
		remaining := max(maxDesc-len(desc), 0)

		// Append as many lines as fit
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		built := &strings.Builder{}
		consumed := 0
		shown := 0
		for _, line := range lines {
			lineLen := len(line) + 1 // include newline
			if consumed+lineLen > remaining {
				break
			}
			built.WriteString(line)
			built.WriteByte('\n')
			consumed += lineLen
			shown++
		}

		desc += built.String()

		// If we couldn't include all items, indicate how many remain
		if shown < len(lines) {
			remainingItems := len(lines) - shown
			more := fmt.Sprintf("…and %d more", remainingItems)
			// Only add the “more” line if it fits
			if len(desc)+len(more)+1 <= maxDesc {
				desc += more
			}
		}
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("Queue%s", loop),
		Description: desc,
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "In queue",
				Value:  queueInfo(p),
				Inline: true,
			},
			{
				Name:   "Total length",
				Value:  totalLenStr(totalLen),
				Inline: true,
			},
			{
				Name:   "Page",
				Value:  fmt.Sprintf("%d out of %d", page, maxPage),
				Inline: true,
			},
			{
				Name: "Resolving",
				Value: func() string {
					if p.IsSearching() {
						return "Yes"
					} else {
						return "No"
					}
				}(),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "kumabot",
		},
	}
	if cur.Thumbnail != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: cur.Thumbnail}
	}
	return embed, nil
}

// QueueComponents returns prev/next page buttons for the queue embed.
// It uses the embed footer text to determine if there are more pages.
func QueueComponents(currentPage int, embed *discordgo.MessageEmbed) []discordgo.MessageComponent {
	hasPrev := currentPage > 1
	hasNext := false
	if embed != nil {
		for _, f := range embed.Fields {
			if f.Name == "Page" {
				var cur, max int
				if _, err := fmt.Sscanf(f.Value, "%d out of %d", &cur, &max); err == nil {
					hasNext = cur < max
				}
			}
		}
	}
	if !hasPrev && !hasNext {
		return nil
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "◀ Prev", Style: discordgo.SecondaryButton, CustomID: "kuma:qprev", Disabled: !hasPrev},
				discordgo.Button{Label: "Next ▶", Style: discordgo.SecondaryButton, CustomID: "kuma:qnext", Disabled: !hasNext},
			},
		},
	}
}

func queueInfo(p *Player) string {
	n := p.QueueSize()
	if n == 0 {
		return "-"
	}
	if n == 1 {
		return "1 song"
	}
	return fmt.Sprintf("%d songs", n)
}

func totalLenStr(sec int) string {
	if sec <= 0 {
		return "-"
	}
	return utils.PrettyTime(sec)
}
