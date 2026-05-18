-- Track every completed song play for Wrapped stats
CREATE TABLE IF NOT EXISTS play_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  guild_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  video_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  artist TEXT NOT NULL DEFAULT '',
  thumbnail TEXT NOT NULL DEFAULT '',
  duration_seconds INTEGER NOT NULL DEFAULT 0,
  played_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_play_history_guild_played ON play_history(guild_id, played_at);
CREATE INDEX IF NOT EXISTS idx_play_history_guild_user ON play_history(guild_id, user_id, played_at);
CREATE INDEX IF NOT EXISTS idx_play_history_guild_video ON play_history(guild_id, video_id, played_at);
CREATE INDEX IF NOT EXISTS idx_play_history_guild_artist ON play_history(guild_id, artist, played_at);
