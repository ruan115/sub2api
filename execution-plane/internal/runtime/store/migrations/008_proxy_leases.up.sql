CREATE TABLE proxy_leases (
    proxy_lease_id VARCHAR(128) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    revoked_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (proxy_lease_id),
    UNIQUE KEY uq_proxy_leases_slot_epoch (slot_id, execution_epoch),
    KEY idx_proxy_leases_account (account_id, revoked_at),
    CONSTRAINT fk_proxy_leases_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT chk_proxy_leases_epoch CHECK (execution_epoch > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
