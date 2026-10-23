-- Model providers a user adds themselves: any endpoint that speaks an
-- OpenAI API, for pi agents. The key is a user secret, "provider:NAME".
CREATE TABLE providers (
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    api            TEXT NOT NULL, -- pi's name for it: openai-responses, openai-completions
    base_url       TEXT NOT NULL,
    models         TEXT NOT NULL, -- space-separated; the first is the default
    images         INTEGER NOT NULL DEFAULT 0,
    reasoning      INTEGER NOT NULL DEFAULT 0,
    context_window INTEGER NOT NULL DEFAULT 0, -- 0 leaves it to pi
    created_at     TEXT NOT NULL,
    PRIMARY KEY (user_id, name)
);
