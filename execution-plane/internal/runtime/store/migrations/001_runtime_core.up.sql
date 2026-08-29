CREATE TABLE nodes (
    node_id VARCHAR(128) NOT NULL,
    status VARCHAR(32) NOT NULL,
    labels_json JSON NOT NULL,
    capabilities_json JSON NOT NULL,
    protocol_major INT UNSIGNED NOT NULL,
    protocol_minor INT UNSIGNED NOT NULL,
    control_session_id VARCHAR(32) NULL,
    max_slots INT UNSIGNED NOT NULL DEFAULT 0,
    max_active_cli INT UNSIGNED NOT NULL DEFAULT 0,
    max_active_api INT UNSIGNED NOT NULL DEFAULT 0,
    max_active_total INT UNSIGNED NOT NULL DEFAULT 0,
    allocatable_cpu_millis BIGINT UNSIGNED NOT NULL DEFAULT 0,
    allocatable_memory_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
    reserved_slots INT UNSIGNED NOT NULL DEFAULT 0,
    reserved_cpu_millis BIGINT UNSIGNED NOT NULL DEFAULT 0,
    reserved_memory_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
    allocated_slots INT UNSIGNED NOT NULL DEFAULT 0,
    active_cli INT UNSIGNED NOT NULL DEFAULT 0,
    active_api INT UNSIGNED NOT NULL DEFAULT 0,
    active_total INT UNSIGNED NOT NULL DEFAULT 0,
    allocated_cpu_millis BIGINT UNSIGNED NOT NULL DEFAULT 0,
    allocated_memory_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
    last_seen_at DATETIME(6) NULL,
    disconnected_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (node_id),
    KEY idx_nodes_status_seen (status, last_seen_at),
    CONSTRAINT chk_nodes_capacity CHECK (
        max_active_cli <= max_active_total AND
        max_active_api <= max_active_total AND
        active_cli <= max_active_cli AND
        active_api <= max_active_api AND
        active_total <= max_active_total AND
        reserved_slots <= max_slots AND
        reserved_cpu_millis <= allocatable_cpu_millis AND
        reserved_memory_bytes <= allocatable_memory_bytes AND
        allocated_slots <= max_slots AND
        allocated_cpu_millis <= allocatable_cpu_millis AND
        allocated_memory_bytes <= allocatable_memory_bytes
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE node_enrollments (
    enrollment_id CHAR(36) NOT NULL,
    token_sha256 BINARY(32) NOT NULL,
    expected_node_id VARCHAR(128) NULL,
    expires_at DATETIME(6) NOT NULL,
    consumed_at DATETIME(6) NULL,
    consumed_by_node_id VARCHAR(128) NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (enrollment_id),
    UNIQUE KEY uq_node_enrollments_token (token_sha256),
    KEY idx_node_enrollments_expiry (expires_at, consumed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE node_certificates (
    serial_number VARCHAR(64) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    certificate_sha256 BINARY(32) NOT NULL,
    public_key_sha256 BINARY(32) NOT NULL,
    status VARCHAR(32) NOT NULL,
    not_before DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    replaced_by_serial VARCHAR(64) NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (serial_number),
    UNIQUE KEY uq_node_certificates_digest (certificate_sha256),
    KEY idx_node_certificates_node_status (node_id, status, expires_at),
    CONSTRAINT fk_node_certificates_node FOREIGN KEY (node_id) REFERENCES nodes(node_id),
    CONSTRAINT fk_node_certificates_replacement FOREIGN KEY (replaced_by_serial) REFERENCES node_certificates(serial_number)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE slots (
    slot_id VARCHAR(128) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    desired_state VARCHAR(32) NOT NULL,
    desired_generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
    next_execution_epoch BIGINT UNSIGNED NOT NULL DEFAULT 1,
    required_labels_json JSON NOT NULL,
    image_digest VARCHAR(512) NOT NULL,
    cpu_request_millis BIGINT UNSIGNED NOT NULL,
    memory_request_bytes BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (slot_id),
    UNIQUE KEY uq_slots_account (account_id),
    KEY idx_slots_desired (desired_state, updated_at),
    CONSTRAINT chk_slots_generation CHECK (desired_generation > 0),
    CONSTRAINT chk_slots_epoch CHECK (next_execution_epoch > 0),
    CONSTRAINT chk_slots_resources CHECK (cpu_request_millis > 0 AND memory_request_bytes > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE slot_assignments (
    assignment_id CHAR(36) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    provider_ref VARCHAR(255) NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    image_digest VARCHAR(512) NOT NULL,
    cpu_request_millis BIGINT UNSIGNED NOT NULL,
    memory_request_bytes BIGINT UNSIGNED NOT NULL,
    actual_state VARCHAR(32) NOT NULL,
    actual_generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
    healthy BOOLEAN NOT NULL DEFAULT FALSE,
    reason_code VARCHAR(64) NOT NULL DEFAULT '',
    assigned_at DATETIME(6) NOT NULL,
    last_observed_at DATETIME(6) NULL,
    released_at DATETIME(6) NULL,
    active_slot_id VARCHAR(128) GENERATED ALWAYS AS (
        CASE WHEN released_at IS NULL THEN slot_id ELSE NULL END
    ) STORED,
    PRIMARY KEY (assignment_id),
    UNIQUE KEY uq_slot_assignments_epoch (slot_id, execution_epoch),
    UNIQUE KEY uq_slot_assignments_active (active_slot_id),
    KEY idx_slot_assignments_node_active (node_id, released_at),
    CONSTRAINT fk_slot_assignments_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT fk_slot_assignments_node FOREIGN KEY (node_id) REFERENCES nodes(node_id),
    CONSTRAINT chk_slot_assignments_epoch CHECK (execution_epoch > 0),
    CONSTRAINT chk_slot_assignments_actual_generation CHECK (actual_generation > 0),
    CONSTRAINT chk_slot_assignments_resources CHECK (cpu_request_millis > 0 AND memory_request_bytes > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE execution_leases (
    lease_id CHAR(36) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    owner_id VARCHAR(128) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (lease_id),
    UNIQUE KEY uq_execution_leases_epoch (slot_id, execution_epoch),
    UNIQUE KEY uq_execution_leases_owner (slot_id, execution_epoch, owner_id),
    KEY idx_execution_leases_expiry (expires_at, revoked_at),
    CONSTRAINT fk_execution_leases_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT fk_execution_leases_node FOREIGN KEY (node_id) REFERENCES nodes(node_id),
    CONSTRAINT chk_execution_leases_epoch CHECK (execution_epoch > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE credential_vault (
    account_id VARCHAR(128) NOT NULL,
    active_version_id CHAR(36) NULL,
    auth_type VARCHAR(32) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE credential_versions (
    version_id CHAR(36) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    version_number BIGINT UNSIGNED NOT NULL,
    ciphertext LONGBLOB NOT NULL,
    encrypted_dek BLOB NOT NULL,
    nonce BINARY(12) NOT NULL,
    aad_json JSON NOT NULL,
    kms_key_id VARCHAR(255) NOT NULL,
    kms_key_version VARCHAR(128) NOT NULL,
    credential_hint VARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (version_id),
    UNIQUE KEY uq_credential_versions_number (account_id, version_number),
    CONSTRAINT fk_credential_versions_vault FOREIGN KEY (account_id) REFERENCES credential_vault(account_id),
    CONSTRAINT chk_credential_versions_number CHECK (version_number > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE credential_leases (
    lease_id CHAR(36) NOT NULL,
    token_sha256 BINARY(32) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    version_id CHAR(36) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    consumed_at DATETIME(6) NULL,
    revoked_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (lease_id),
    UNIQUE KEY uq_credential_leases_token (token_sha256),
    KEY idx_credential_leases_expiry (expires_at, consumed_at, revoked_at),
    CONSTRAINT fk_credential_leases_version FOREIGN KEY (version_id) REFERENCES credential_versions(version_id),
    CONSTRAINT fk_credential_leases_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT chk_credential_leases_epoch CHECK (execution_epoch > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE runtime_sessions (
    session_hash BINARY(32) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    mode VARCHAR(32) NOT NULL,
    state VARCHAR(32) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (session_hash),
    KEY idx_runtime_sessions_expiry (expires_at, state),
    CONSTRAINT fk_runtime_sessions_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT chk_runtime_sessions_epoch CHECK (execution_epoch > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE provisioning_jobs (
    job_id CHAR(36) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    desired_generation BIGINT UNSIGNED NOT NULL,
    step VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL,
    retry_count INT UNSIGNED NOT NULL DEFAULT 0,
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    next_attempt_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (job_id),
    UNIQUE KEY uq_provisioning_jobs_idempotency (idempotency_key),
    KEY idx_provisioning_jobs_retry (status, next_attempt_at),
    CONSTRAINT fk_provisioning_jobs_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT chk_provisioning_jobs_generation CHECK (desired_generation > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE image_releases (
    release_id CHAR(36) NOT NULL,
    image_digest VARCHAR(512) NOT NULL,
    claude_cli_version VARCHAR(64) NOT NULL,
    channel_name VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL,
    sbom_sha256 BINARY(32) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (release_id),
    UNIQUE KEY uq_image_releases_digest (image_digest),
    KEY idx_image_releases_channel_status (channel_name, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE node_drain_jobs (
    drain_job_id CHAR(36) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    status VARCHAR(32) NOT NULL,
    deadline DATETIME(6) NOT NULL,
    force_terminate BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (drain_job_id),
    KEY idx_node_drain_jobs_node_status (node_id, status),
    CONSTRAINT fk_node_drain_jobs_node FOREIGN KEY (node_id) REFERENCES nodes(node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE node_command_results (
    command_id VARCHAR(128) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    succeeded BOOLEAN NOT NULL,
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    error_message VARCHAR(1024) NOT NULL DEFAULT '',
    slot_observation_json JSON NULL,
    received_at DATETIME(6) NOT NULL,
    PRIMARY KEY (command_id),
    KEY idx_node_command_results_node_received (node_id, received_at),
    CONSTRAINT fk_node_command_results_node FOREIGN KEY (node_id) REFERENCES nodes(node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE reconciliation_runs (
    run_id CHAR(36) NOT NULL,
    scope VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL,
    scanned_count INT UNSIGNED NOT NULL DEFAULT 0,
    changed_count INT UNSIGNED NOT NULL DEFAULT 0,
    error_count INT UNSIGNED NOT NULL DEFAULT 0,
    started_at DATETIME(6) NOT NULL,
    finished_at DATETIME(6) NULL,
    PRIMARY KEY (run_id),
    KEY idx_reconciliation_runs_started (started_at, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
