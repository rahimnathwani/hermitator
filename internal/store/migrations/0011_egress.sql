-- Keep credentials out of sandboxes: agents hold placeholders, and the
-- egress proxy swaps in real values on the way to each secret's hosts.
ALTER TABLE users ADD COLUMN egress INTEGER NOT NULL DEFAULT 0;

-- Where each of a user's own secrets may go, for the egress proxy. A
-- secret without hosts reaches the sandbox as it is.
CREATE TABLE secret_hosts (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    hosts   TEXT NOT NULL, -- space-separated host names
    PRIMARY KEY (user_id, name)
);
