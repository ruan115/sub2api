CREATE TABLE credential_rotation_commits (
    credential_lease_id VARCHAR(128) NOT NULL,
    material_sha256 BINARY(32) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    proxy_lease_id VARCHAR(128) NOT NULL,
    credential_version_id CHAR(36) NULL,
    authorized_at DATETIME(6) NOT NULL,
    committed_at DATETIME(6) NULL,
    PRIMARY KEY (credential_lease_id),
    UNIQUE KEY uq_credential_rotation_commits_version (credential_version_id),
    KEY idx_credential_rotation_commits_account (account_id, authorized_at),
    CONSTRAINT fk_credential_rotation_commits_workflow_lease FOREIGN KEY (credential_lease_id) REFERENCES onboarding_workflows(credential_lease_id),
    CONSTRAINT fk_credential_rotation_commits_version FOREIGN KEY (credential_version_id) REFERENCES credential_versions(version_id),
    CONSTRAINT fk_credential_rotation_commits_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT chk_credential_rotation_commits_epoch CHECK (execution_epoch > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
