CREATE TABLE IF NOT EXISTS linear_oauth (
  id              INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton
  access_token    TEXT NOT NULL,
  refresh_token   TEXT,
  expires_at      TEXT,  -- RFC3339 UTC, may be empty if server didn't return one
  updated_at      TEXT NOT NULL
);

INSERT OR IGNORE INTO linear_oauth (id, access_token, refresh_token, expires_at, updated_at)
VALUES (1, '', '', '', '');
