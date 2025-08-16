package player

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/sponsorblock"
	"github.com/sonroyaalmerol/kumabot/internal/spotify"
	"github.com/sonroyaalmerol/kumabot/internal/stream"
	"github.com/sonroyaalmerol/kumabot/internal/utils"
)

var (
	sbOnce    sync.Once
	sbApplier *sponsorblock.Applier
)

func getSB(cfg *config.Config) *sponsorblock.Applier {
	sbOnce.Do(func() {
		if cfg.EnableSponsorBlock {
			sbApplier = sponsorblock.NewApplier(cfg.SponsorBlockTimeoutMin)
		}
	})
	return sbApplier
}

func ResolveQueryToSongs(ctx context.Context, cfg *config.Config, query string, playlistLimit int, split bool) ([]SongMetadata, string, error) {
	q := strings.TrimSpace(query)
	extra := ""

	// URL/URI branch
	if strings.HasPrefix(q, "http://") || strings.HasPrefix(q, "https://") || strings.HasPrefix(q, "spotify:") {
		// Spotify URL/URI?
		if isSpotify(q) {
			if cfg.SpotifyClientID == "" || cfg.SpotifyClientSecret == "" {
				return nil, "", fmt.Errorf("spotify is not enabled")
			}
			sp, err := spotify.NewClientCredentials(cfg.SpotifyClientID, cfg.SpotifyClientSecret)
			if err != nil {
				return nil, "", fmt.Errorf("spotify auth: %w", err)
			}
			typ, id, err := spotify.ParseID(q)
			if err != nil {
				return nil, "", fmt.Errorf("invalid spotify identifier")
			}

			switch typ {
			case "album":
				tracks, qp, err := sp.GetAlbum(ctx, id, playlistLimit)
				if err != nil {
					return nil, "", err
				}
				return spotifyTracksToYouTube(ctx, cfg, tracks, &QueuedPlaylist{Title: qp.Title, Source: qp.Source}, playlistLimit, split)

			case "playlist":
				tracks, qp, err := sp.GetPlaylist(ctx, id, playlistLimit)
				if err != nil {
					return nil, "", err
				}
				if playlistLimit > 0 && len(tracks) > playlistLimit {
					extra = fmt.Sprintf("a random sample of %d songs was taken", playlistLimit)
				}
				songs, notFoundMsg, err := spotifyTracksToYouTube(ctx, cfg, tracks, &QueuedPlaylist{Title: qp.Title, Source: qp.Source}, playlistLimit, split)
				if err != nil {
					return nil, "", err
				}
				if notFoundMsg != "" {
					if extra != "" {
						extra += " and "
					}
					extra += notFoundMsg
				}
				return songs, extra, nil

			case "track":
				t, err := sp.GetTrack(ctx, id)
				if err != nil {
					return nil, "", err
				}
				songs, notFoundMsg, err := spotifyTracksToYouTube(ctx, cfg, []spotify.Track{t}, nil, 1, split)
				if err != nil {
					return nil, "", err
				}
				extra = notFoundMsg
				return songs, extra, nil

			case "artist":
				tracks, err := sp.GetArtistTop(ctx, id, "US", playlistLimit)
				if err != nil {
					return nil, "", err
				}
				return spotifyTracksToYouTube(ctx, cfg, tracks, nil, playlistLimit, split)

			default:
				return nil, "", fmt.Errorf("unsupported spotify type: %s", typ)
			}
		}

		// YouTube?
		if strings.Contains(q, "youtube.com") || strings.Contains(q, "youtu.be") || strings.Contains(q, "music.youtube.") {
			if strings.Contains(q, "list=") {
				infos, err := stream.YtdlpPlaylist(ctx, q)
				if err != nil || len(infos) == 0 {
					return nil, "", fmt.Errorf("not found")
				}
				if len(infos) > playlistLimit {
					extra = fmt.Sprintf("a random sample of %d songs was taken", playlistLimit)
					utils.ShuffleSlice(infos)
					infos = infos[:playlistLimit]
				}
				var out []SongMetadata
				for _, info := range infos {
					tracks := ytInfoToMetadata(info, nil, split)
					for _, t := range tracks {
						applySponsorBlockToSong(ctx, cfg, &t)
						out = append(out, t)
					}
				}
				return out, extra, nil
			}
			info, err := stream.YtdlpGetInfo(ctx, q)
			if err != nil {
				return nil, "", err
			}
			tracks := ytInfoToMetadata(info, nil, split)
			for i := range tracks {
				applySponsorBlockToSong(ctx, cfg, &tracks[i])
			}
			return tracks, "", nil
		}

		// Fallback: HLS or other radio URL
		return []SongMetadata{{
			Title:     q,
			Artist:    q,
			URL:       q,
			Length:    0,
			Offset:    0,
			Playlist:  nil,
			IsLive:    true,
			Thumbnail: "",
			Source:    SourceHLS,
		}}, "", nil
	}

	// Not a URL or URI => YouTube search
	info, err := stream.YtdlpGetInfo(ctx, "ytsearch1:"+q)
	if err != nil {
		return nil, "", err
	}
	tracks := ytInfoToMetadata(info, nil, split)
	for i := range tracks {
		applySponsorBlockToSong(ctx, cfg, &tracks[i])
	}
	return tracks, "", nil
}

func isSpotify(s string) bool {
	return strings.HasPrefix(s, "spotify:") || strings.Contains(s, "open.spotify.com")
}

func applySponsorBlockToSong(ctx context.Context, cfg *config.Config, m *SongMetadata) {
	if !cfg.EnableSponsorBlock || m.Source != SourceYouTube || m.IsLive {
		return
	}
	sb := getSB(cfg)
	if sb == nil {
		return
	}
	newLen, newOff, _, changed := sb.Adjust(ctx, m.URL, m.Length, m.Offset)
	if changed {
		m.Length = newLen
		m.Offset = newOff
	}
}

func spotifyTracksToYouTube(
	ctx context.Context,
	cfg *config.Config,
	tracks []spotify.Track,
	qp *QueuedPlaylist,
	playlistLimit int,
	split bool,
) ([]SongMetadata, string, error) {
	if len(tracks) == 0 {
		return nil, "", nil
	}
	if playlistLimit > 0 && len(tracks) > playlistLimit {
		utils.ShuffleSlice(tracks)
		tracks = tracks[:playlistLimit]
	}

	out := make([]SongMetadata, 0, len(tracks))
	notFound := 0

	for _, t := range tracks {
		q := fmt.Sprintf(`ytsearch1:"%s" "%s"`, t.Name, t.Artist)
		info, err := stream.YtdlpGetInfo(ctx, q)
		if err != nil || info == nil {
			notFound++
			continue
		}
		tracks := ytInfoToMetadata(info, qp, split)
		for _, t := range tracks {
			applySponsorBlockToSong(ctx, cfg, &t)
			out = append(out, t)
		}
	}

	extra := ""
	if notFound > 0 {
		if notFound == 1 {
			extra = "1 song was not found"
		} else {
			extra = fmt.Sprintf("%d songs were not found", notFound)
		}
	}
	return out, extra, nil
}

func ytInfoToMetadata(info *stream.YTDLPInfo, qp *QueuedPlaylist, split bool) []SongMetadata {
	thumb := ""
	if len(info.Thumbnails) > 0 {
		thumb = info.Thumbnails[len(info.Thumbnails)-1].Url
	}

	// Base song
	base := SongMetadata{
		Title:     info.Title,
		Artist:    info.Uploader,
		URL:       info.Id, // YouTube ID
		Length:    max(0, info.Duration),
		Offset:    0,
		Playlist:  qp,
		IsLive:    info.IsLive,
		Thumbnail: thumb,
		Source:    SourceYouTube,
	}

	// Only split when requested and not live and description exists
	if split && !base.IsLive && info.Description != "" && base.Length > 0 {
		chapters := parseChaptersFromDescription(info.Description, base.Length)
		if len(chapters) > 0 {
			out := make([]SongMetadata, 0, len(chapters))
			for _, ch := range chapters {
				sm := base
				sm.Offset = ch.Offset
				sm.Length = ch.Length
				sm.Title = ch.Label + " (" + base.Title + ")"
				out = append(out, sm)
			}
			return out
		}
	}

	return []SongMetadata{base}
}
