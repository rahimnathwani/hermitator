-- A task's check: a shell command run in the sandbox at each time, which
-- starts the task's turn only if it prints something. check_failing
-- remembers that the owner was told it fails, so they are told once.
ALTER TABLE tasks ADD COLUMN check_command TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN check_failing INTEGER NOT NULL DEFAULT 0;
