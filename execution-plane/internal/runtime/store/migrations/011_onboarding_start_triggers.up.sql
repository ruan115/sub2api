CREATE TABLE onboarding_start_triggers (
    trigger_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    source_sequence BIGINT UNSIGNED NOT NULL,
    event_id VARCHAR(128) NOT NULL,
    event_type VARCHAR(96) NOT NULL,
    event_created_at DATETIME(6) NOT NULL,
    intent_id CHAR(36) NOT NULL,
    intent_expires_at DATETIME(6) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    desired_generation BIGINT UNSIGNED NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    required_labels_json JSON NOT NULL,
    image_digest VARCHAR(512) NOT NULL,
    cpu_request_millis BIGINT UNSIGNED NOT NULL,
    memory_request_bytes BIGINT UNSIGNED NOT NULL,
    reservation_id VARCHAR(128) NOT NULL,
    binding_revision BIGINT UNSIGNED NOT NULL,
    status VARCHAR(16) NOT NULL,
    claim_owner VARCHAR(128) NOT NULL DEFAULT '',
    claim_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
    claim_expires_at DATETIME(6) NULL,
    next_attempt_at DATETIME(6) NOT NULL,
    attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    last_error_code VARCHAR(64) NOT NULL DEFAULT '',
    started_workflow_id VARCHAR(128) NOT NULL DEFAULT '',
    started_at DATETIME(6) NULL,
    projected_at DATETIME(6) NOT NULL,
    PRIMARY KEY (trigger_id),
    UNIQUE KEY uq_onboarding_start_triggers_event (event_id),
    UNIQUE KEY uq_onboarding_start_triggers_sequence (source_sequence),
    UNIQUE KEY uq_onboarding_start_triggers_intent (intent_id),
    UNIQUE KEY uq_onboarding_start_triggers_account_generation (account_id, desired_generation),
    KEY idx_onboarding_start_triggers_due (status, next_attempt_at, source_sequence, intent_id),
    CONSTRAINT fk_onboarding_start_triggers_intent FOREIGN KEY (intent_id) REFERENCES onboarding_intents(intent_id),
    CONSTRAINT fk_onboarding_start_triggers_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT fk_onboarding_start_triggers_reservation FOREIGN KEY (
        reservation_id, account_id, desired_generation, binding_revision
    ) REFERENCES proxy_reservation_grants (
        reservation_id, account_id, desired_generation, binding_revision
    ),
    CONSTRAINT chk_onboarding_start_triggers_sequence CHECK (source_sequence > 0),
    CONSTRAINT chk_onboarding_start_triggers_generation CHECK (desired_generation > 0),
    CONSTRAINT chk_onboarding_start_triggers_resources CHECK (cpu_request_millis > 0 AND memory_request_bytes > 0),
    CONSTRAINT chk_onboarding_start_triggers_revision CHECK (binding_revision > 0),
    CONSTRAINT chk_onboarding_start_triggers_event_type CHECK (event_type IN (
        'account.runtime.provision_requested',
        'account.credential.migrate_requested',
        'account.credential.rotate_requested'
    )),
    CONSTRAINT chk_onboarding_start_triggers_status CHECK (status IN ('pending', 'claimed', 'started', 'expired')),
    CONSTRAINT chk_onboarding_start_triggers_state CHECK (
        (status = 'pending' AND claim_owner = '' AND claim_expires_at IS NULL
            AND started_workflow_id = '' AND started_at IS NULL) OR
        (status = 'expired' AND claim_owner = '' AND claim_expires_at IS NULL
            AND started_workflow_id = '' AND started_at IS NULL) OR
        (status = 'claimed' AND claim_owner <> '' AND claim_version > 0 AND claim_expires_at IS NOT NULL
            AND started_workflow_id = '' AND started_at IS NULL) OR
        (status = 'started' AND claim_owner <> '' AND claim_version > 0 AND claim_expires_at IS NOT NULL
            AND started_workflow_id <> '' AND started_at IS NOT NULL)
    ),
    CONSTRAINT chk_onboarding_start_triggers_timestamps CHECK (
        intent_expires_at > projected_at AND event_created_at <= projected_at
        AND next_attempt_at >= projected_at
        AND (started_at IS NULL OR (started_at < intent_expires_at AND started_at < claim_expires_at))
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
