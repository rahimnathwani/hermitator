-- Which messages in a linked chat make its agent work, separately for the
-- owner and for everyone else: 'all', 'mentions' or 'none'. Messages that
-- do not trigger the agent are held (below) and handed over with the next
-- one that does.
ALTER TABLE routes ADD COLUMN owner_trigger TEXT NOT NULL DEFAULT 'all';
ALTER TABLE routes ADD COLUMN others_trigger TEXT NOT NULL DEFAULT 'none';
UPDATE routes SET others_trigger = CASE WHEN require_mention = 1 THEN 'mentions' ELSE 'none' END;
ALTER TABLE routes DROP COLUMN require_mention;

-- Messages in a linked chat that did not trigger its agent. Attachments are
-- already saved in the agent's inbox; media_file names them there.
CREATE TABLE held_messages (
    id          TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL,
    chat_jid    TEXT NOT NULL,
    message_id  TEXT NOT NULL,
    sender      TEXT NOT NULL,
    sender_name TEXT NOT NULL DEFAULT '',
    from_owner  INTEGER NOT NULL DEFAULT 0,
    text        TEXT NOT NULL DEFAULT '',
    media_kind  TEXT NOT NULL DEFAULT '',
    media_mime  TEXT NOT NULL DEFAULT '',
    media_file  TEXT NOT NULL DEFAULT '',
    voice       INTEGER NOT NULL DEFAULT 0,
    sent_at     TEXT NOT NULL,
    FOREIGN KEY (account_id, chat_jid) REFERENCES routes (account_id, chat_jid) ON DELETE CASCADE
);

CREATE INDEX held_messages_chat ON held_messages (account_id, chat_jid, sent_at);
