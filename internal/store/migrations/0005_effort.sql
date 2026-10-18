-- How hard the harness thinks: Claude Code's --effort or pi's --thinking.
-- Empty means the harness's own default.
ALTER TABLE agents ADD COLUMN effort TEXT NOT NULL DEFAULT '';

-- The model a turn actually ran on, as the harness reported it.
ALTER TABLE turns ADD COLUMN model TEXT NOT NULL DEFAULT '';
