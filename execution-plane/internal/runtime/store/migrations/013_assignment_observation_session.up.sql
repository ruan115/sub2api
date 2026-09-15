-- Historical observations remain unproven until a fresh authenticated command
-- result establishes the current control-session identity. Never backfill it.
ALTER TABLE slot_assignments
  ADD COLUMN observed_control_session_id VARCHAR(32) NULL AFTER last_observed_at;
