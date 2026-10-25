-- One-off messages hermitator has sent a user, such as a warning that a
-- token is about to expire, so each goes once.
CREATE TABLE notices (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind    TEXT NOT NULL,
    sent_at TEXT NOT NULL,
    PRIMARY KEY (user_id, kind)
);
