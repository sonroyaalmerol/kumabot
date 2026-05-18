-- Persist guild-level player state across bot restarts
CREATE TABLE IF NOT EXISTS guild_state (
  guild_id TEXT PRIMARY KEY,
  radio_mode INTEGER NOT NULL DEFAULT 0,
  shuffle_mode INTEGER NOT NULL DEFAULT 0,
  loop_song INTEGER NOT NULL DEFAULT 0,
  loop_queue INTEGER NOT NULL DEFAULT 0,
  voice_channel_id TEXT NOT NULL DEFAULT '',
  text_channel_id TEXT NOT NULL DEFAULT ''
);

-- Persist the song queue for guilds so it can be restored after restarts
CREATE TABLE IF NOT EXISTS guild_queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  guild_id TEXT NOT NULL,
  position INTEGER NOT NULL,
  video_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  artist TEXT NOT NULL DEFAULT '',
  thumbnail TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  source INTEGER NOT NULL DEFAULT 0,
  requested_by TEXT NOT NULL DEFAULT '',
  length INTEGER NOT NULL DEFAULT 0,
  offset INTEGER NOT NULL DEFAULT 0,
  is_live INTEGER NOT NULL DEFAULT 0,
  playlist_title TEXT NOT NULL DEFAULT '',
  playlist_source TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_guild_queue_guild ON guild_queue(guild_id, position);
