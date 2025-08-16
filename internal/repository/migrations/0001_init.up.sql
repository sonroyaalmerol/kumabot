-- settings table
CREATE TABLE IF NOT EXISTS settings (
  guild_id TEXT PRIMARY KEY,
  playlist_limit INTEGER NOT NULL DEFAULT 50,
  seconds_wait_after_empty INTEGER NOT NULL DEFAULT 30,
  leave_if_no_listeners INTEGER NOT NULL DEFAULT 1,
  queue_add_ephemeral INTEGER NOT NULL DEFAULT 0,
  auto_announce_next_song INTEGER NOT NULL DEFAULT 1,
  default_volume INTEGER NOT NULL DEFAULT 100,
  default_queue_page_size INTEGER NOT NULL DEFAULT 10,
  turn_down_when_speaking INTEGER NOT NULL DEFAULT 0,
  turn_down_target INTEGER NOT NULL DEFAULT 50
);

-- favorites table
CREATE TABLE IF NOT EXISTS favorites (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  guild_id TEXT NOT NULL,
  author_id TEXT NOT NULL,
  name TEXT NOT NULL,
  query TEXT NOT NULL,
  UNIQUE (guild_id, name)
);

-- file_cache table
CREATE TABLE IF NOT EXISTS file_cache (
  hash TEXT PRIMARY KEY,
  bytes INTEGER NOT NULL,
  accessed_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
