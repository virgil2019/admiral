-- One row per Linear agent session admiral picks up. Keyed on
-- agent_session_id (Linear's AgentSession UUID) because the same issue can
-- spawn multiple sessions over time (e.g. user assigns, finishes, then
-- mentions admiral again later — that's two sessions on one issue).
--
-- state transitions: RECEIVED -> EXECUTING -> DONE | FAILED
--   RECEIVED   webhook accepted, worktree not yet created
--   EXECUTING  worktree created, claude -p running (or finished, awaiting PR check)
--   DONE       PR exists, response activity posted into the agent thread
--   FAILED     terminal failure (worktree, claude, or PR creation gave up);
--              an error activity has been posted into the agent thread
CREATE TABLE IF NOT EXISTS autopilot_jobs (
  agent_session_id TEXT PRIMARY KEY,
  issue_id         TEXT NOT NULL,
  issue_identifier TEXT,
  state            TEXT NOT NULL,
  worktree_path    TEXT,
  branch           TEXT,
  pr_url           TEXT,
  error            TEXT,
  started_at       TEXT NOT NULL,
  finished_at      TEXT
);

CREATE INDEX IF NOT EXISTS autopilot_jobs_issue_id_idx
  ON autopilot_jobs(issue_id);
