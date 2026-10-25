-- Users an admin lets run /host from WhatsApp. Admins own no WhatsApp
-- account, so the permission belongs to an ordinary user.
ALTER TABLE users ADD COLUMN host_operator INTEGER NOT NULL DEFAULT 0;
