CREATE TABLE proxy_reservation_grants (
    reservation_id VARCHAR(128) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    desired_generation BIGINT UNSIGNED NOT NULL,
    proxy_binding_id VARCHAR(128) NOT NULL,
    binding_revision BIGINT UNSIGNED NOT NULL,
    grant_event_id VARCHAR(128) NOT NULL,
    revoke_event_id VARCHAR(128) NULL,
    revoked_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (reservation_id),
    UNIQUE KEY uq_proxy_reservation_account_generation (account_id, desired_generation),
    UNIQUE KEY uq_proxy_reservation_grant_event (grant_event_id),
    UNIQUE KEY uq_proxy_reservation_revoke_event (revoke_event_id),
    UNIQUE KEY uq_proxy_reservation_runtime_binding (reservation_id, account_id, desired_generation, binding_revision),
    CONSTRAINT chk_proxy_reservation_generation CHECK (desired_generation > 0),
    CONSTRAINT chk_proxy_reservation_revision CHECK (binding_revision > 0),
    CONSTRAINT chk_proxy_reservation_revocation CHECK (
        (revoke_event_id IS NULL AND revoked_at IS NULL) OR
        (revoke_event_id IS NOT NULL AND revoked_at IS NOT NULL)
    ),
    CONSTRAINT chk_proxy_reservation_timestamps CHECK (
        updated_at >= created_at AND (revoked_at IS NULL OR revoked_at >= created_at)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

ALTER TABLE slot_assignments
    ADD COLUMN desired_generation BIGINT UNSIGNED NULL AFTER execution_epoch,
    ADD CONSTRAINT chk_slot_assignments_desired_generation CHECK (desired_generation > 0);

ALTER TABLE proxy_leases
    ADD COLUMN reservation_id VARCHAR(128) NULL AFTER proxy_lease_id,
    ADD COLUMN desired_generation BIGINT UNSIGNED NULL AFTER account_id,
    ADD COLUMN binding_revision BIGINT UNSIGNED NULL AFTER desired_generation,
    ADD KEY idx_proxy_leases_reservation (reservation_id),
    ADD CONSTRAINT fk_proxy_leases_reservation_binding FOREIGN KEY (
        reservation_id, account_id, desired_generation, binding_revision
    ) REFERENCES proxy_reservation_grants (
        reservation_id, account_id, desired_generation, binding_revision
    ),
    ADD CONSTRAINT chk_proxy_leases_generation CHECK (desired_generation > 0),
    ADD CONSTRAINT chk_proxy_leases_binding_revision CHECK (binding_revision > 0);
