package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

func (r *Repo) UpsertSettings(ctx context.Context, guild string) (*Settings, error) {
	_, _ = r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO settings(guild_id) VALUES (?)`, guild,
	)
	return r.GetSettings(ctx, guild)
}

func (r *Repo) GetSettings(ctx context.Context, guild string) (*Settings, error) {
	row := r.db.QueryRowContext(ctx, `
	SELECT guild_id, playlist_limit, seconds_wait_after_empty, leave_if_no_listeners,
	       queue_add_ephemeral, auto_announce_next_song, default_volume,
	       default_queue_page_size, turn_down_when_speaking, turn_down_target
	FROM settings WHERE guild_id = ?`, guild)

	var s Settings
	var b1, b2, b3, b4 int
	if err := row.Scan(
		&s.GuildID,
		&s.PlaylistLimit,
		&s.SecondsWaitAfterEmpty,
		&b1, // leave_if_no_listeners
		&b2, // queue_add_ephemeral
		&b4, // auto_announce_next_song
		&s.DefaultVolume,
		&s.DefaultQueuePageSize,
		&b3, // turn_down_when_speaking
		&s.TurnDownTarget,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}

	s.LeaveIfNoListeners = b1 != 0
	s.QAddEphemeral = b2 != 0
	s.AutoAnnounceNext = b4 != 0
	s.TurnDownWhenSpeaking = b3 != 0
	return &s, nil
}

func (r *Repo) UpdateSettings(ctx context.Context, s *Settings) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE settings SET
		  playlist_limit=?,
		  seconds_wait_after_empty=?,
		  leave_if_no_listeners=?,
		  queue_add_ephemeral=?,
		  auto_announce_next_song=?,
		  default_volume=?,
		  default_queue_page_size=?,
		  turn_down_when_speaking=?,
		  turn_down_target=?
		WHERE guild_id=?`,
		s.PlaylistLimit, s.SecondsWaitAfterEmpty, boolToInt(s.LeaveIfNoListeners),
		boolToInt(s.QAddEphemeral), boolToInt(s.AutoAnnounceNext), s.DefaultVolume,
		s.DefaultQueuePageSize, boolToInt(s.TurnDownWhenSpeaking),
		s.TurnDownTarget, s.GuildID,
	)
	return err
}

func (r *Repo) AddFavorite(ctx context.Context, f *Favorite) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO favorites(guild_id, author_id, name, query) VALUES (?,?,?,?)`,
		f.GuildID, f.Author, f.Name, f.Query,
	)
	return err
}

func (r *Repo) RemoveFavorite(ctx context.Context, guild, name string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM favorites WHERE guild_id=? AND name=?`, guild, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *Repo) FindFavorite(ctx context.Context, guild, name string) (*Favorite, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, guild_id, author_id, name, query FROM favorites WHERE guild_id=? AND name=?`, guild, name)
	var f Favorite
	if err := row.Scan(&f.ID, &f.GuildID, &f.Author, &f.Name, &f.Query); err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *Repo) ListFavorites(ctx context.Context, guild string) ([]Favorite, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, guild_id, author_id, name, query FROM favorites WHERE guild_id=? ORDER BY name ASC`, guild)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Favorite
	for rows.Next() {
		var f Favorite
		if err := rows.Scan(&f.ID, &f.GuildID, &f.Author, &f.Name, &f.Query); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func (r *Repo) CacheTouch(ctx context.Context, hash string, size int64, created bool) error {
	now := time.Now().Unix()
	if created {
		_, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO file_cache(hash,bytes,accessed_at,created_at) VALUES (?,?,?,COALESCE((SELECT created_at FROM file_cache WHERE hash=?),?))`,
			hash, size, now, hash, now)
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE file_cache SET accessed_at=? WHERE hash=?`, now, hash)
	return err
}

func (r *Repo) CacheRemove(ctx context.Context, hash string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM file_cache WHERE hash=?`, hash)
	return err
}

func (r *Repo) CacheTotalBytes(ctx context.Context) (int64, error) {
	row := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM file_cache`)
	var v int64
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

func (r *Repo) CacheOldest(ctx context.Context) (string, error) {
	row := r.db.QueryRowContext(ctx, `SELECT hash FROM file_cache ORDER BY accessed_at ASC LIMIT 1`)
	var hash string
	if err := row.Scan(&hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (r *Repo) RecordPlay(ctx context.Context, e *PlayHistoryEntry) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO play_history(guild_id, user_id, video_id, title, artist, thumbnail, duration_seconds, played_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.GuildID, e.UserID, e.VideoID, e.Title, e.Artist, e.Thumbnail, e.DurationSeconds, e.PlayedAt,
	)
	return err
}

func (r *Repo) GetWrappedStats(ctx context.Context, guildID string, since int64) (*WrappedStats, error) {
	s := &WrappedStats{}

	// Total plays and duration
	row := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(duration_seconds), 0) FROM play_history WHERE guild_id = ? AND played_at >= ?`,
		guildID, since,
	)
	if err := row.Scan(&s.TotalPlays, &s.TotalDuration); err != nil {
		return nil, err
	}

	if s.TotalPlays == 0 {
		return s, nil
	}

	// Top songs
	rows, err := r.db.QueryContext(ctx,
		`SELECT title, artist, video_id, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND played_at >= ?
		 GROUP BY video_id ORDER BY cnt DESC LIMIT 5`,
		guildID, since,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TopSong
		if err := rows.Scan(&t.Title, &t.Artist, &t.VideoID, &t.PlayCount); err != nil {
			rows.Close()
			return nil, err
		}
		s.TopSongs = append(s.TopSongs, t)
	}
	rows.Close()

	// Top artists
	rows, err = r.db.QueryContext(ctx,
		`SELECT artist, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND played_at >= ? AND artist != ''
		 GROUP BY artist ORDER BY cnt DESC LIMIT 5`,
		guildID, since,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TopArtist
		if err := rows.Scan(&t.Artist, &t.PlayCount); err != nil {
			rows.Close()
			return nil, err
		}
		s.TopArtists = append(s.TopArtists, t)
	}
	rows.Close()

	// Top users
	rows, err = r.db.QueryContext(ctx,
		`SELECT user_id, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND played_at >= ?
		 GROUP BY user_id ORDER BY cnt DESC LIMIT 5`,
		guildID, since,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TopUser
		if err := rows.Scan(&t.UserID, &t.PlayCount); err != nil {
			rows.Close()
			return nil, err
		}
		s.TopUsers = append(s.TopUsers, t)
	}
	rows.Close()

	// Peak hour (0-23)
	row = r.db.QueryRowContext(ctx,
		`SELECT (played_at / 3600 %% 24) as hour, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND played_at >= ?
		 GROUP BY hour ORDER BY cnt DESC LIMIT 1`,
		guildID, since,
	)
	var peakHour int
	var peakHourCount int
	if err := row.Scan(&peakHour, &peakHourCount); err == nil {
		s.PeakHour = peakHour
	}

	// Hourly counts for all 24 hours
	rows, err = r.db.QueryContext(ctx,
		`SELECT (played_at / 3600 %% 24) as hour, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND played_at >= ?
		 GROUP BY hour ORDER BY hour`,
		guildID, since,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h HourlyCount
		if err := rows.Scan(&h.Hour, &h.Count); err != nil {
			rows.Close()
			return nil, err
		}
		s.HourlyCounts = append(s.HourlyCounts, h)
	}
	rows.Close()

	// Peak day of week and daily counts
	row = r.db.QueryRowContext(ctx,
		`SELECT CASE cast(strftime('%%w', datetime(played_at, 'unixepoch')) AS INTEGER)
			WHEN 0 THEN 'Sunday' WHEN 1 THEN 'Monday' WHEN 2 THEN 'Tuesday'
			WHEN 3 THEN 'Wednesday' WHEN 4 THEN 'Thursday' WHEN 5 THEN 'Friday'
			WHEN 6 THEN 'Saturday' END as day, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND played_at >= ?
		 GROUP BY day ORDER BY cnt DESC LIMIT 1`,
		guildID, since,
	)
	if err := row.Scan(&s.PeakDay, new(int)); err != nil {
		s.PeakDay = ""
	}

	return s, nil
}

func (r *Repo) GetUserWrappedStats(ctx context.Context, guildID, userID string, since int64) (*WrappedStats, error) {
	s := &WrappedStats{}

	row := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(duration_seconds), 0) FROM play_history WHERE guild_id = ? AND user_id = ? AND played_at >= ?`,
		guildID, userID, since,
	)
	if err := row.Scan(&s.TotalPlays, &s.TotalDuration); err != nil {
		return nil, err
	}

	if s.TotalPlays == 0 {
		return s, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT title, artist, video_id, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND user_id = ? AND played_at >= ?
		 GROUP BY video_id ORDER BY cnt DESC LIMIT 5`,
		guildID, userID, since,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TopSong
		if err := rows.Scan(&t.Title, &t.Artist, &t.VideoID, &t.PlayCount); err != nil {
			rows.Close()
			return nil, err
		}
		s.TopSongs = append(s.TopSongs, t)
	}
	rows.Close()

	rows, err = r.db.QueryContext(ctx,
		`SELECT artist, COUNT(*) as cnt
		 FROM play_history WHERE guild_id = ? AND user_id = ? AND played_at >= ? AND artist != ''
		 GROUP BY artist ORDER BY cnt DESC LIMIT 5`,
		guildID, userID, since,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TopArtist
		if err := rows.Scan(&t.Artist, &t.PlayCount); err != nil {
			rows.Close()
			return nil, err
		}
		s.TopArtists = append(s.TopArtists, t)
	}
	rows.Close()

	return s, nil
}

// --- Guild State ---

func (r *Repo) LoadGuildState(ctx context.Context, guildID string) (*GuildState, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT guild_id, radio_mode, shuffle_mode, loop_song, loop_queue, voice_channel_id, text_channel_id
		 FROM guild_state WHERE guild_id = ?`, guildID)

	var s GuildState
	var rMode, shuffle, lSong, lQueue int
	if err := row.Scan(&s.GuildID, &rMode, &shuffle, &lSong, &lQueue, &s.VoiceChanID, &s.TextChanID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	s.RadioMode = rMode != 0
	s.ShuffleMode = shuffle != 0
	s.LoopSong = lSong != 0
	s.LoopQueue = lQueue != 0
	return &s, nil
}

func (r *Repo) SaveGuildState(ctx context.Context, s *GuildState) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO guild_state(guild_id, radio_mode, shuffle_mode, loop_song, loop_queue, voice_channel_id, text_channel_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(guild_id) DO UPDATE SET
		   radio_mode=excluded.radio_mode, shuffle_mode=excluded.shuffle_mode,
		   loop_song=excluded.loop_song, loop_queue=excluded.loop_queue,
		   voice_channel_id=excluded.voice_channel_id, text_channel_id=excluded.text_channel_id`,
		s.GuildID, boolToInt(s.RadioMode), boolToInt(s.ShuffleMode),
		boolToInt(s.LoopSong), boolToInt(s.LoopQueue),
		s.VoiceChanID, s.TextChanID,
	)
	return err
}

func (r *Repo) SaveGuildQueue(ctx context.Context, guildID string, qpos int, songs []QueuedSong) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, _ = tx.ExecContext(ctx, `DELETE FROM guild_queue WHERE guild_id = ?`, guildID)

	for i, s := range songs {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO guild_queue(guild_id, position, video_id, title, artist, thumbnail, url, source, requested_by, length, offset, is_live, playlist_title, playlist_source, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			guildID, i, s.VideoID, s.Title, s.Artist, s.Thumbnail, s.URL, s.Source,
			s.RequestedBy, s.Length, s.Offset, boolToInt(s.IsLive),
			s.PlaylistTitle, s.PlaylistSource, time.Now().Unix(),
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (r *Repo) LoadGuildQueue(ctx context.Context, guildID string) ([]QueuedSong, int, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, guild_id, position, video_id, title, artist, thumbnail, url, source, requested_by, length, offset, is_live, playlist_title, playlist_source
		 FROM guild_queue WHERE guild_id = ? ORDER BY position ASC`, guildID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var songs []QueuedSong
	for rows.Next() {
		var s QueuedSong
		var isLive int
		if err := rows.Scan(&s.ID, &s.GuildID, &s.Position, &s.VideoID, &s.Title, &s.Artist,
			&s.Thumbnail, &s.URL, &s.Source, &s.RequestedBy, &s.Length, &s.Offset,
			&isLive, &s.PlaylistTitle, &s.PlaylistSource); err != nil {
			return nil, 0, err
		}
		s.IsLive = isLive != 0
		songs = append(songs, s)
	}

	// Find the first entry that hasn't been played (position >= saved qpos)
	// The "qpos" is stored as the position of the first item in the queue.
	// For simplicity, we always resume from the beginning of saved songs.
	return songs, 0, nil
}

func (r *Repo) ClearGuildQueue(ctx context.Context, guildID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM guild_queue WHERE guild_id = ?`, guildID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
