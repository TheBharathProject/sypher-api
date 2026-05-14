-- Tracks which Sypher tools each user has accessed and when.
-- first_seen = first ever login via that tool.
-- last_seen  = most recent login (updated on every OAuth callback).
-- One row per (user, tool) pair — composite PK prevents duplicates.

CREATE TABLE IF NOT EXISTS auth.user_tool_access (
  user_id    UUID        NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
  tool       TEXT        NOT NULL,  -- 'pegasus' | 'kairos' | future tools
  first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (user_id, tool)
);

CREATE INDEX IF NOT EXISTS user_tool_access_tool_idx
  ON auth.user_tool_access (tool, last_seen DESC);
