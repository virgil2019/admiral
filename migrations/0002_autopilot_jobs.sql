-- Tracks one row per Linear issue admiral picks up. issue_id is the Linear
-- issue UUID (webhook payload data.id), unique by design — single-flight v0.3
-- already prevents two concurrent jobs, so PK on issue_id is fine.
--
-- state transitions: RECEIVED -> EXECUTING -> DONE | FAILED
--   RECEIVED   webhook accepted, worktree not yet created
--   EXECUTING  worktree created, claude -p running (or finished, awaiting PR check)
--   DONE       PR exists, Linear status flipped to done, closing comment posted
--   FAILED     terminal failure (worktree, claude, or PR creation gave up)
CREATE TABLE IF NOT EXISTS autopilot_jobs (
  issue_id        TEXT PRIMARY KEY,
  issue_identifier TEXT,
  state           TEXT NOT NULL,
  worktree_path   TEXT,
  branch          TEXT,
  pr_url          TEXT,
  error           TEXT,
  started_at      TEXT NOT NULL,
  finished_at     TEXT
);
