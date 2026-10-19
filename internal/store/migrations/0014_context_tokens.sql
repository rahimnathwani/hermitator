-- How many tokens a turn's last model call read: what the next turn
-- re-reads, at full price after a long sleep. The agent's copy is the
-- latest, or 0 once compacted or after /new.
ALTER TABLE turns ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agents ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
