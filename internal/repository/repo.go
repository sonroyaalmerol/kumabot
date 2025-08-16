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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
