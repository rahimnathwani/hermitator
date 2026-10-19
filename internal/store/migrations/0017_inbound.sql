-- Messages as they arrive from WhatsApp, written before WhatsApp is told
-- they were received. One that no turn has taken yet when hermitator
-- stops is handed over again when it is back; one WhatsApp delivers twice
-- is recognised by its ID. Rows are kept a while after they are done, for
-- that recognition, then pruned.
CREATE TABLE inbound (
    account_id  TEXT NOT NULL,
    message_id  TEXT NOT NULL,
    raw         BLOB NOT NULL,
    done        INTEGER NOT NULL DEFAULT 0,
    received_at TEXT NOT NULL,
    PRIMARY KEY (account_id, message_id)
);
CREATE INDEX inbound_pending ON inbound (account_id, done);
