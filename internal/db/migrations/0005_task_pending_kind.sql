-- Which kind of pause a waiting_for_input task is sitting on:
-- '' (not paused), 'question' (free-text ask_user prompt), or 'permission'
-- (tool-permission approve/deny prompt). Status alone cannot tell them apart,
-- so a client reconnecting mid-pause — or landing on a different pod — had no
-- way to know whether to render a text box or approve/deny buttons.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pending_kind TEXT;
