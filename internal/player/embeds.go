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

func BuildPlayingEmbed(p *Player) *discordgo.MessageEmbed {
	const maxDesc = 4096

	cur := p.GetCurrent()
	if cur == nil {
		return &discordgo.MessageEmbed{
			Title:       "Nothing Playing",
			Description: "No playing song found",
			Color:       0x992222,
		}
	}

	p.mu.Lock()
	pos := p.PositionSec
	status := p.Status
	loopSong := p.LoopSong
	loopQueue := p.LoopQueue
	shuffle := p.ShuffleMode
	radio := p.RadioMode
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

	color := 0x006400
	title := "Now Playing"
	if status != StatusPlaying {
		color = 0x8B0000
		title = "Paused"
	}

	footer := fmt.Sprintf("Source: %s", cur.Artist)
	if cur.Playlist != nil {
		footer += " (" + cur.Playlist.Title + ")"
	}
	footer += " • kumabot"

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

// PlayingComponents returns the playback control button row for the now-playing embed.
func PlayingComponents(p *Player) []discordgo.MessageComponent {
	pauseLabel := "Pause"
	if p.StatusPub() != StatusPlaying {
		pauseLabel = "Resume"
	}
	loopStyle := discordgo.SecondaryButton
	if p.LoopSongPub() {
		loopStyle = discordgo.PrimaryButton
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "⏮", Style: discordgo.SecondaryButton, CustomID: "kuma:prev"},
				discordgo.Button{Label: pauseLabel, Style: discordgo.PrimaryButton, CustomID: "kuma:pause"},
				discordgo.Button{Label: "⏭", Style: discordgo.SecondaryButton, CustomID: "kuma:next"},
				discordgo.Button{Label: "🔁", Style: loopStyle, CustomID: "kuma:loop"},
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
	for _, s := range p.Queue() {
		totalLen += s.Length
	}

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
	if p.LoopSong {
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
		Title:       fmt.Sprintf("Now Playing%s", loop),
		Description: desc,
		Color:       0x006400,
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
			Text: fmt.Sprintf("Source: %s %s", cur.Artist, func() string {
				if cur.Playlist != nil {
					return "(" + cur.Playlist.Title + ")"
				}
				return ""
			}()),
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

