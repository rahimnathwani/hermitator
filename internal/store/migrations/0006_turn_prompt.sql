-- The whole prompt a turn sent, so /retry can send it again. The input
-- column keeps a clipped copy for display.
ALTER TABLE turns ADD COLUMN prompt TEXT NOT NULL DEFAULT '';
