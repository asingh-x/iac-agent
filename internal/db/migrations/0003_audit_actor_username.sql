ALTER TABLE audit_events DROP CONSTRAINT audit_events_actor_id_fkey;
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS actor_username TEXT;
