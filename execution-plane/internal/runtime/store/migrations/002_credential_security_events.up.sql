CREATE TABLE credential_security_events (
    event_id CHAR(36) NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    reason_code VARCHAR(64) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    lease_id CHAR(36) NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (event_id),
    KEY idx_credential_security_events_account_time (account_id, created_at),
    KEY idx_credential_security_events_slot_epoch (slot_id, execution_epoch, created_at),
    CONSTRAINT chk_credential_security_events_epoch CHECK (execution_epoch > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
