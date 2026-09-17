CREATE TABLE runtime_certificates (
    assignment_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    account_hash CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    slot_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    node_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    execution_epoch BIGINT UNSIGNED NOT NULL,
    runtime_generation BIGINT UNSIGNED NOT NULL,
    public_key_sha256 BINARY(32) NOT NULL,
    ca_sha256 BINARY(32) NOT NULL,
    not_before DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    certificate_pem VARBINARY(16384) NOT NULL,
    PRIMARY KEY (assignment_id),
    UNIQUE KEY uq_runtime_certificates_slot_epoch (slot_id, execution_epoch)
) ENGINE=InnoDB;
