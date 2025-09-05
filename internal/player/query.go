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

type ResolveEvent struct {
	Song SongMetadata
	Info string
	Err  error
}

func ResolveQueryStream(
	ctx context.Context,
	cfg *config.Config,
	query string,
	playlistLimit int,
	split bool,
) <-chan ResolveEvent {
	ch := make(chan ResolveEvent, 8)

	go func() {
		defer close(ch)
		q := strings.TrimSpace(query)

		// URL/URI branch
		if strings.HasPrefix(q, "http://") || strings.HasPrefix(q, "https://") || strings.HasPrefix(q, "spotify:") {
			// Spotify
			if isSpotify(q) {
				if cfg.SpotifyClientID == "" || cfg.SpotifyClientSecret == "" {
					ch <- ResolveEvent{Err: fmt.Errorf("spotify is not enabled")}
					return
				}
				sp, err := spotify.NewClientCredentials(cfg.SpotifyClientID, cfg.SpotifyClientSecret)
				if err != nil {
					ch <- ResolveEvent{Err: fmt.Errorf("spotify auth: %w", err)}
					return
				}
				typ, id, err := spotify.ParseID(q)
				if err != nil {
					ch <- ResolveEvent{Err: fmt.Errorf("invalid spotify identifier")}
					return
				}

				switch typ {
				case "album":
					tracks, qp, err := sp.GetAlbum(ctx, id, playlistLimit)
					if err != nil {
						ch <- ResolveEvent{Err: err}
						return
					}
					streamSpotifyTracks(ctx, cfg, tracks, &QueuedPlaylist{Title: qp.Title, Source: qp.Source}, playlistLimit, split, ch)
					return

				case "playlist":
					tracks, qp, err := sp.GetPlaylist(ctx, id, playlistLimit)
					if err != nil {
						ch <- ResolveEvent{Err: err}
						return
					}
					if playlistLimit > 0 && len(tracks) > playlistLimit {
						ch <- ResolveEvent{Info: fmt.Sprintf("a random sample of %d songs was taken", playlistLimit)}
					}
					streamSpotifyTracks(ctx, cfg, tracks, &QueuedPlaylist{Title: qp.Title, Source: qp.Source}, playlistLimit, split, ch)
					return

				case "track":
					t, err := sp.GetTrack(ctx, id)
					if err != nil {
						ch <- ResolveEvent{Err: err}
						return
					}
					streamSpotifyTracks(ctx, cfg, []spotify.Track{t}, nil, 1, split, ch)
					return

				case "artist":
					tracks, err := sp.GetArtistTop(ctx, id, "US", playlistLimit)
					if err != nil {
						ch <- ResolveEvent{Err: err}
						return
					}
					streamSpotifyTracks(ctx, cfg, tracks, nil, playlistLimit, split, ch)
					return

				default:
					ch <- ResolveEvent{Err: fmt.Errorf("unsupported spotify type: %s", typ)}
					return
				}
			}

			// YouTube playlist
			if strings.Contains(q, "youtube.com") || strings.Contains(q, "youtu.be") || strings.Contains(q, "music.youtube.") {
				if strings.Contains(q, "list=") {
					infos, err := stream.YtdlpPlaylist(ctx, q)
					if err != nil || len(infos) == 0 {
						ch <- ResolveEvent{Err: fmt.Errorf("not found")}
						return
					}
					if playlistLimit > 0 && len(infos) > playlistLimit {
						utils.ShuffleSlice(infos)
						infos = infos[:playlistLimit]
						ch <- ResolveEvent{Info: fmt.Sprintf("a random sample of %d songs was taken", playlistLimit)}
					}
					// resolve each entry incrementally
					for _, e := range infos {
						select {
						case <-ctx.Done():
							return
						default:
						}
						// fetch full info to get streamable URL and description
						info, err := stream.YtdlpGetInfo(ctx, "https://www.youtube.com/watch?v="+e.Id)
						if err != nil || info == nil {
							ch <- ResolveEvent{Err: fmt.Errorf("failed to get info for %s", e.Id)}
							continue
						}
						tracks := ytInfoToMetadata(info, nil, split)
						for i := range tracks {
							applySponsorBlockToSong(ctx, cfg, &tracks[i])
							ch <- ResolveEvent{Song: tracks[i]}
						}
					}
					return
				}

				// single YouTube item
				info, err := stream.YtdlpGetInfo(ctx, q)
				if err != nil {
					ch <- ResolveEvent{Err: err}
					return
				}
				tracks := ytInfoToMetadata(info, nil, split)
				for i := range tracks {
					applySponsorBlockToSong(ctx, cfg, &tracks[i])
					ch <- ResolveEvent{Song: tracks[i]}
				}
				return
			}

			// Fallback: HLS or radio URL
			ch <- ResolveEvent{Song: SongMetadata{
				Title:     q,
				Artist:    q,
				URL:       q,
				Length:    0,
				Offset:    0,
				Playlist:  nil,
				IsLive:    true,
				Thumbnail: "",
				Source:    SourceHLS,
			}}
			return
		}

		// Not a URL => YouTube search
		info, err := stream.YtdlpGetInfo(ctx, "ytsearch1:"+q)
		if err != nil {
			ch <- ResolveEvent{Err: err}
			return
		}
		tracks := ytInfoToMetadata(info, nil, split)
		for i := range tracks {
			applySponsorBlockToSong(ctx, cfg, &tracks[i])
			ch <- ResolveEvent{Song: tracks[i]}
		}
	}()

	return ch
}

// helper to turn spotify tracks into SongMetadata and stream them
func streamSpotifyTracks(
	ctx context.Context,
	cfg *config.Config,
	tracks []spotify.Track,
	qp *QueuedPlaylist,
	playlistLimit int,
	split bool,
	ch chan<- ResolveEvent,
) {
	if len(tracks) == 0 {
		return
	}
	if playlistLimit > 0 && len(tracks) > playlistLimit {
		utils.ShuffleSlice(tracks)
		tracks = tracks[:playlistLimit]
	}

	for _, t := range tracks {
		select {
		case <-ctx.Done():
			return
		default:
		}
		q := fmt.Sprintf(`ytsearch1:"%s" "%s"`, t.Name, t.Artist)
		info, err := stream.YtdlpGetInfo(ctx, q)
		if err != nil || info == nil {
			ch <- ResolveEvent{Err: fmt.Errorf("not found: %s - %s", t.Artist, t.Name)}
			continue
		}
		mts := ytInfoToMetadata(info, qp, split)
		for i := range mts {
			applySponsorBlockToSong(ctx, cfg, &mts[i])
			ch <- ResolveEvent{Song: mts[i]}
		}
	}
}

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
		URL:       info.Url,
		Length:    max(0, int(info.Duration)),
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
