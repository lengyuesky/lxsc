PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS users (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT NOT NULL UNIQUE,
  password_enc TEXT NOT NULL,           -- AES-GCM 加密后的口令（Subsonic token 认证需要明文）
  is_admin     INTEGER NOT NULL DEFAULT 0,
  quality      TEXT NOT NULL DEFAULT '320k',
  created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  key        TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sources (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  version     TEXT NOT NULL DEFAULT '',
  author      TEXT NOT NULL DEFAULT '',
  homepage    TEXT NOT NULL DEFAULT '',
  script      TEXT NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  priority    INTEGER NOT NULL DEFAULT 100,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- 见过的歌曲元数据（搜索/歌单/榜单结果），json 为 SDK 返回的原始 musicInfo
CREATE TABLE IF NOT EXISTS tracks (
  id         TEXT PRIMARY KEY,
  source     TEXT NOT NULL,
  name       TEXT NOT NULL DEFAULT '',
  singer     TEXT NOT NULL DEFAULT '',
  album      TEXT NOT NULL DEFAULT '',
  json       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tracks_singer ON tracks(singer);

CREATE TABLE IF NOT EXISTS playlists (
  id         TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  comment    TEXT NOT NULL DEFAULT '',
  public     INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS playlist_tracks (
  playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  track_id    TEXT NOT NULL,
  PRIMARY KEY (playlist_id, position)
);

CREATE TABLE IF NOT EXISTS stars (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    TEXT NOT NULL,
  kind       TEXT NOT NULL,   -- track / album / artist
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, item_id)
);

CREATE TABLE IF NOT EXISTS history (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  track_id  TEXT NOT NULL,
  played_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_history_user ON history(user_id, played_at DESC);

-- 专辑/歌手元数据缓存
CREATE TABLE IF NOT EXISTS albums (
  id         TEXT PRIMARY KEY,
  source     TEXT NOT NULL,
  name       TEXT NOT NULL DEFAULT '',
  artist     TEXT NOT NULL DEFAULT '',
  json       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS artists (
  id         TEXT PRIMARY KEY,
  source     TEXT NOT NULL,
  name       TEXT NOT NULL DEFAULT '',
  json       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
