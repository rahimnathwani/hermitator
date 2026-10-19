-- Preview links: a public URL for a port in an agent's sandbox. A link's
-- secret is derived from the master key and the nonce, so none is stored;
-- deleting the row revokes every link to it.
CREATE TABLE previews (
    agent_id   TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    port       INTEGER NOT NULL,
    nonce      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_id, port)
);
