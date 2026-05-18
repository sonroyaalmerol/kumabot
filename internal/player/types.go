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
	Playlist          *QueuedPlaylist
	URL               string
	VideoID           string
	Title             string
	Artist            string
	Thumbnail         string
	RequestedBy       string
	AddedInChan       string
	Length            int
	Offset            int
	Source            MediaSource
	IsLive            bool
	IsRadioSuggestion bool
}

type PlayerStatus int

const (
	StatusPlaying PlayerStatus = iota
	StatusPaused
	StatusIdle
)
