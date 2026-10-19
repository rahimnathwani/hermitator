-- Turns with a user message since the agent last reviewed its
-- conversation for lasting facts; a review runs before sleep once there
-- are enough.
ALTER TABLE agents ADD COLUMN review_turns INTEGER NOT NULL DEFAULT 0;
