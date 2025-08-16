package ui

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
	"github.com/sonroyaalmerol/kumabot/internal/player"
	"github.com/sonroyaalmerol/kumabot/internal/utils"
)

func songLink(s player.SongMetadata) string {
	if s.Source == player.SourceHLS {
		return fmt.Sprintf("[%s](%s)", s.Title, s.URL)
	}
	yid := s.URL
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

func BuildPlayingEmbed(p *player.Player) *discordgo.MessageEmbed {
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
	if p.Status == player.StatusPlaying {
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
	if p.Status != player.StatusPlaying {
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
	p *player.Player,
	page int,
	pageSize int,
	ongoingQueue bool,
) (*discordgo.MessageEmbed, error) {
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
	if ongoingQueue {
		desc += "Note: still resolving more items… they will appear here as they’re ready.\n\n"
	}

	if len(items) > 0 {
		desc += "**Up next:**\n" + out
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
					if ongoingQueue {
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

func queueInfo(p *player.Player) string {
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
