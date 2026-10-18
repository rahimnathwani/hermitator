-- Who the owner's agents commit as, and the GitHub account their token
-- signs in as (shown in Settings; the token itself is a secret).
ALTER TABLE users ADD COLUMN git_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN git_email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN github_login TEXT NOT NULL DEFAULT '';
