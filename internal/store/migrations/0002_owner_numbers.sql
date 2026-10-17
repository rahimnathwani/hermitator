-- Phone numbers that belong to a user. Messages from them count as the
-- user's own: they can run chat commands and reach the fallback agent.
-- This is what makes a dedicated agent number usable, where the owner
-- writes from a different phone.
CREATE TABLE owner_numbers (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    phone      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (user_id, phone)
);

-- 'agent': a number that belongs to the agents. hermitator shows it as
-- online, sends read receipts and typing indicators.
-- 'personal': the owner's own number. hermitator stays out of sight.
ALTER TABLE whatsapp_accounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'agent';
