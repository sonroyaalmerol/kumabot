package player

type MediaSource int

const (
	SourceYouTube MediaSource = iota
	SourceHLS
)

type QueuedPlaylist struct {
	Title  string
	Source string
}

type SongMetadata struct {
	Title       string
	Artist      string
	VideoID     string
	URL         string // youtube videoId or full HLS URL
	Length      int    // seconds
	Offset      int    // seconds to start
	Playlist    *QueuedPlaylist
	IsLive      bool
	Thumbnail   string
	Source      MediaSource
	RequestedBy string
	AddedInChan string
}

type PlayerStatus int

const (
	StatusPlaying PlayerStatus = iota
	StatusPaused
	StatusIdle
)
