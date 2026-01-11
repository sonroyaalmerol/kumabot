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
	cur := p.GetCurrent()
	if cur == nil {
		return &discordgo.MessageEmbed{
			Title:       "Nothing Playing",
			Description: "No playing song found",
			Color:       0x992222,
		}
	}
	pos := p.GetPosition()
	button := "▶️"
	if p.Status == StatusPlaying {
		button = "⏹️"
	}
	progress := 0.0
	if cur.Length > 0 {
		progress = float64(pos) / float64(cur.Length)
	}
	bar := ProgressBar(10, progress)
	elapsed := "live"
	if !cur.IsLive {
		elapsed = fmt.Sprintf("%s/%s", utils.PrettyTime(pos), utils.PrettyTime(cur.Length))
	}
	loop := ""
	if p.LoopSong {
		loop = "🔂"
	} else if p.LoopQueue {
		loop = "🔁"
	}

	desc := fmt.Sprintf("**%s**\nRequested by: <@%s>\n\n%s %s `[ %s ]` %s",
		songLink(*cur),
		cur.RequestedBy,
		button, bar, elapsed, loop,
	)

	color := 0x006400
	title := "Now Playing"
	if p.Status != StatusPlaying {
		color = 0x8B0000
		title = "Paused"
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: desc,
		Color:       color,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Source: %s", cur.Artist),
		},
	}
	if cur.Thumbnail != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: cur.Thumbnail}
	}
	return embed
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
	out := ""
	begin := (page - 1) * pageSize
	for idx, s := range items {
		n := begin + idx + 1
		dur := "live"
		if !s.IsLive {
			dur = utils.PrettyTime(s.Length)
		}
		out += fmt.Sprintf("`%d.` %s `[ %s ]`\n", n, songLink(s), dur)
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
		remaining := maxDesc - len(desc)
		if remaining < 0 {
			remaining = 0
		}

		// Append as many lines as fit
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
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
