package repository

import "database/sql"

type Repo struct {
	db *sql.DB
}

type Settings struct {
	GuildID               string
	PlaylistLimit         int
	SecondsWaitAfterEmpty int
	LeaveIfNoListeners    bool
	QAddEphemeral         bool
	AutoAnnounceNext      bool
	DefaultVolume         int
	DefaultQueuePageSize  int
	TurnDownWhenSpeaking  bool
	TurnDownTarget        int
}

type Favorite struct {
	ID      int64
	GuildID string
	Author  string
	Name    string
	Query   string
}

