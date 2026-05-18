package handlers

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	plib "github.com/sonroyaalmerol/kumabot/internal/player"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
	"github.com/sonroyaalmerol/kumabot/internal/utils"
)

type listeningPersonality struct {
	Name        string
	Emoji       string
	Description string
}

func getPersonality(stats *repository.WrappedStats) listeningPersonality {
	if stats.TotalPlays < 10 {
		return listeningPersonality{
			Name:        "The Casual Listener",
			Emoji:       "🎧",
			Description: "You dip in and out — quality over quantity. Every song is a choice.",
		}
	}

	songsPerArtist := float64(0)
	if len(stats.TopArtists) > 0 {
		songsPerArtist = float64(stats.TotalPlays) / float64(len(stats.TopArtists))
	}

	if stats.TotalPlays >= 500 {
		if len(stats.TopArtists) > 0 && songsPerArtist > 20 {
			return listeningPersonality{
				Name:        "The Loyalist",
				Emoji:       "👑",
				Description: fmt.Sprintf("You stick with what you love. Your top artist alone accounts for a huge chunk of your %d plays.", stats.TotalPlays),
			}
		}
		if stats.PeakHour >= 22 || stats.PeakHour < 4 {
			return listeningPersonality{
				Name:        "The Night Owl",
				Emoji:       "🦉",
				Description: "The best music happens after midnight. Your peak listening hour says it all.",
			}
		}
		if stats.PeakHour >= 6 && stats.PeakHour < 12 {
			return listeningPersonality{
				Name:        "The Early Bird",
				Emoji:       "🐦",
				Description: "You rise with the music. Morning vibes are your weapon of choice.",
			}
		}
		return listeningPersonality{
			Name:        "The Connoisseur",
			Emoji:       "🏆",
			Description: fmt.Sprintf("%d songs and counting. You don't just listen to music — you experience it.", stats.TotalPlays),
		}
	}

	if stats.TotalPlays >= 100 {
		if len(stats.TopSongs) > 0 && stats.TopSongs[0].PlayCount > 10 {
			return listeningPersonality{
				Name:        "The Obsessed",
				Emoji:       "🔁",
				Description: fmt.Sprintf("You played **%s** %d times. No judgment... okay, maybe a little.", stats.TopSongs[0].Title, stats.TopSongs[0].PlayCount),
			}
		}
		return listeningPersonality{
			Name:        "The Explorer",
			Emoji:       "🧭",
			Description: "A hundred songs in and you're just getting started. Your taste knows no borders.",
		}
	}

	if len(stats.TopArtists) == 1 {
		return listeningPersonality{
			Name:        "The Devotee",
			Emoji:       "❤️",
			Description: "One artist. One love. You know what you like and you own it.",
		}
	}

	return listeningPersonality{
		Name:        "The Eclectic",
		Emoji:       "🎨",
		Description: "A little bit of everything. Your playlists are a journey through sound.",
	}
}

func buildWrappedEmbeds(guildName string, stats *repository.WrappedStats) []*discordgo.MessageEmbed {
	var embeds []*discordgo.MessageEmbed

	// --- Slide 1: Header ---
	headerDesc := fmt.Sprintf(
		"## 🎵 %s Wrapped\n\n"+
			"Here's your server's year in music!\n\n"+
			"📊 **%d** songs played\n"+
			"⏱️ **%s** of total listening time",
		guildName,
		stats.TotalPlays,
		utils.PrettyTime(stats.TotalDuration),
	)
	embeds = append(embeds, &discordgo.MessageEmbed{
		Title:       "🎶 Your Year in Music",
		Description: headerDesc,
		Color:       plib.ColorWrappedHeader,
	})

	if stats.TotalPlays == 0 {
		embeds[0].Description += "\n\n*Not enough data yet! Keep listening and come back later.*"
		return embeds
	}

	// --- Slide 2: Top Songs ---
	var songLines []string
	for i, s := range stats.TopSongs {
		medal := ""
		switch i {
		case 0:
			medal = "🥇"
		case 1:
			medal = "🥈"
		case 2:
			medal = "🥉"
		default:
			medal = fmt.Sprintf("**%d.**", i+1)
		}
		artist := ""
		if s.Artist != "" {
			artist = fmt.Sprintf(" by *%s*", s.Artist)
		}
		songLines = append(songLines, fmt.Sprintf("%s **%s**%s — %d plays", medal, s.Title, artist, s.PlayCount))
	}
	songsDesc := "## 🔥 Top Songs\n\n" + strings.Join(songLines, "\n")
	embeds = append(embeds, &discordgo.MessageEmbed{
		Title:       "🔥 Top Songs",
		Description: songsDesc,
		Color:       plib.ColorWrappedSongs,
	})

	// --- Slide 3: Top Artists ---
	if len(stats.TopArtists) > 0 {
		var artistLines []string
		for i, a := range stats.TopArtists {
			medal := ""
			switch i {
			case 0:
				medal = "🥇"
			case 1:
				medal = "🥈"
			case 2:
				medal = "🥉"
			default:
				medal = fmt.Sprintf("**%d.**", i+1)
			}
			artistLines = append(artistLines, fmt.Sprintf("%s **%s** — %d plays", medal, a.Artist, a.PlayCount))
		}
		artistsDesc := "## 🎤 Top Artists\n\n" + strings.Join(artistLines, "\n")
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title:       "🎤 Top Artists",
			Description: artistsDesc,
			Color:       plib.ColorWrappedArtists,
		})
	}

	// --- Slide 4: Top DJs (Users) ---
	if len(stats.TopUsers) > 0 {
		var djLines []string
		for i, u := range stats.TopUsers {
			medal := ""
			switch i {
			case 0:
				medal = "🥇"
			case 1:
				medal = "🥈"
			case 2:
				medal = "🥉"
			default:
				medal = fmt.Sprintf("**%d.**", i+1)
			}
			djLines = append(djLines, fmt.Sprintf("%s <@%s> — %d songs queued", medal, u.UserID, u.PlayCount))
		}
		djsDesc := "## 🎧 Top DJs\n\n" + strings.Join(djLines, "\n")
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title:       "🎧 Top DJs",
			Description: djsDesc,
			Color:       plib.ColorWrappedDJs,
		})
	}

	// --- Slide 5: Listening Habits ---
	personality := getPersonality(stats)
	habitsLines := []string{
		fmt.Sprintf("🕐 Peak listening hour: **%d:00**", stats.PeakHour),
	}
	if stats.PeakDay != "" {
		habitsLines = append(habitsLines, fmt.Sprintf("📅 Busiest day: **%s**", stats.PeakDay))
	}
	habitsDesc := fmt.Sprintf(
		"## 🧬 Listening Personality\n\n"+
			"%s **%s**\n%s\n\n%s",
		personality.Emoji,
		personality.Name,
		personality.Description,
		strings.Join(habitsLines, "\n"),
	)
	embeds = append(embeds, &discordgo.MessageEmbed{
		Title:       "🧬 Your Listening DNA",
		Description: habitsDesc,
		Color:       plib.ColorWrappedHabits,
	})

	// --- Slide 6: Activity Graph (text-based bar chart) ---
	if len(stats.HourlyCounts) > 0 {
		maxCount := 0
		for _, h := range stats.HourlyCounts {
			if h.Count > maxCount {
				maxCount = h.Count
			}
		}
		barBlocks := []string{"", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
		var graphLines []string
		graphLines = append(graphLines, "```")
		// Build a 24-hour bar chart
		for hour := range 24 {
			count := 0
			for _, h := range stats.HourlyCounts {
				if h.Hour == hour {
					count = h.Count
					break
				}
			}
			barIdx := 0
			if maxCount > 0 {
				barIdx = int(float64(count) / float64(maxCount) * float64(len(barBlocks)-2))
				barIdx++ // offset by 1 since index 0 is empty
			}
			if barIdx >= len(barBlocks) {
				barIdx = len(barBlocks) - 1
			}
			graphLines = append(graphLines, fmt.Sprintf("%02d:00 %s %d", hour, barBlocks[barIdx], count))
		}
		graphLines = append(graphLines, "```")
		graphDesc := "## 📊 Listening Activity (by Hour)\n\n" + strings.Join(graphLines, "\n")
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title:       "📊 Listening Activity",
			Description: graphDesc,
			Color:       plib.ColorWrappedActivity,
		})
	}

	return embeds
}

func buildUserWrappedEmbeds(userName string, stats *repository.WrappedStats) []*discordgo.MessageEmbed {
	var embeds []*discordgo.MessageEmbed

	headerDesc := fmt.Sprintf(
		"## 🎵 %s's Wrapped\n\n"+
			"Here's your personal year in music!\n\n"+
			"📊 **%d** songs played\n"+
			"⏱️ **%s** of total listening time",
		userName,
		stats.TotalPlays,
		utils.PrettyTime(stats.TotalDuration),
	)
	embeds = append(embeds, &discordgo.MessageEmbed{
		Title:       "🎶 Your Wrapped",
		Description: headerDesc,
		Color:       plib.ColorWrappedHeader,
	})

	if stats.TotalPlays == 0 {
		embeds[0].Description += "\n\n*You haven't queued any songs yet! Start listening to build your stats.*"
		return embeds
	}

	// Top Songs
	if len(stats.TopSongs) > 0 {
		var songLines []string
		for i, s := range stats.TopSongs {
			medal := ""
			switch i {
			case 0:
				medal = "🥇"
			case 1:
				medal = "🥈"
			case 2:
				medal = "🥉"
			default:
				medal = fmt.Sprintf("**%d.**", i+1)
			}
			artist := ""
			if s.Artist != "" {
				artist = fmt.Sprintf(" by *%s*", s.Artist)
			}
			songLines = append(songLines, fmt.Sprintf("%s **%s**%s — %d plays", medal, s.Title, artist, s.PlayCount))
		}
		songsDesc := "## 🔥 Your Top Songs\n\n" + strings.Join(songLines, "\n")
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title:       "🔥 Your Top Songs",
			Description: songsDesc,
			Color:       plib.ColorWrappedSongs,
		})
	}

	// Top Artists
	if len(stats.TopArtists) > 0 {
		var artistLines []string
		for i, a := range stats.TopArtists {
			medal := ""
			switch i {
			case 0:
				medal = "🥇"
			case 1:
				medal = "🥈"
			case 2:
				medal = "🥉"
			default:
				medal = fmt.Sprintf("**%d.**", i+1)
			}
			artistLines = append(artistLines, fmt.Sprintf("%s **%s** — %d plays", medal, a.Artist, a.PlayCount))
		}
		artistsDesc := "## 🎤 Your Top Artists\n\n" + strings.Join(artistLines, "\n")
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title:       "🎤 Your Top Artists",
			Description: artistsDesc,
			Color:       plib.ColorWrappedArtists,
		})
	}

	// Personality
	personality := getPersonality(stats)
	personalityDesc := fmt.Sprintf(
		"## 🧬 Your Listening Personality\n\n"+
			"%s **%s**\n%s",
		personality.Emoji,
		personality.Name,
		personality.Description,
	)
	embeds = append(embeds, &discordgo.MessageEmbed{
		Title:       "🧬 Your Listening Personality",
		Description: personalityDesc,
		Color:       plib.ColorWrappedHabits,
	})

	return embeds
}
