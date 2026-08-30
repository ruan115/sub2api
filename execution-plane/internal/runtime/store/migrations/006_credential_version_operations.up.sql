CREATE TABLE credential_version_operations (
    operation_id VARCHAR(128) NOT NULL,
    version_id CHAR(36) NOT NULL,
    account_id VARCHAR(128) NOT NULL,
    auth_type VARCHAR(32) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (operation_id),
    UNIQUE KEY uq_credential_version_operations_version (version_id),
    KEY idx_credential_version_operations_account (account_id, created_at),
    CONSTRAINT fk_credential_version_operations_version FOREIGN KEY (version_id) REFERENCES credential_versions(version_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
