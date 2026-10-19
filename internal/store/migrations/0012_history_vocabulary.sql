-- Words voice notes should get right (names, jargon), as whisper.cpp's
-- initial prompt.
ALTER TABLE agents ADD COLUMN vocabulary TEXT NOT NULL DEFAULT '';

-- Keep a chat's messages in a file the agent can search.
ALTER TABLE routes ADD COLUMN history INTEGER NOT NULL DEFAULT 0;
