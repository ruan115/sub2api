CREATE TABLE lifecycle_event_apply_receipts (
    receipt_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    source_sequence BIGINT UNSIGNED NOT NULL,
    event_id VARCHAR(128) NOT NULL,
    event_account_id BIGINT UNSIGNED NOT NULL,
    event_type VARCHAR(96) NOT NULL,
    desired_generation BIGINT UNSIGNED NOT NULL,
    event_payload_sha256 BINARY(32) NOT NULL,
    event_created_at DATETIME(6) NOT NULL,
    slot_id VARCHAR(128) NOT NULL,
    slot_account_id VARCHAR(128) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    desired_state VARCHAR(32) NOT NULL,
    required_labels_json JSON NOT NULL,
    image_digest VARCHAR(512) NOT NULL,
    cpu_request_millis BIGINT UNSIGNED NOT NULL,
    memory_request_bytes BIGINT UNSIGNED NOT NULL,
    applied_at DATETIME(6) NOT NULL,
    PRIMARY KEY (receipt_id),
    UNIQUE KEY uq_lifecycle_event_apply_receipts_event (event_id),
    UNIQUE KEY uq_lifecycle_event_apply_receipts_sequence (source_sequence),
    KEY idx_lifecycle_event_apply_receipts_slot (slot_id, desired_generation),
    CONSTRAINT fk_lifecycle_event_apply_receipts_slot FOREIGN KEY (slot_id) REFERENCES slots(slot_id),
    CONSTRAINT chk_lifecycle_event_apply_receipts_sequence CHECK (source_sequence > 0),
    CONSTRAINT chk_lifecycle_event_apply_receipts_account CHECK (event_account_id > 0),
    CONSTRAINT chk_lifecycle_event_apply_receipts_generation CHECK (desired_generation > 0),
    CONSTRAINT chk_lifecycle_event_apply_receipts_resources CHECK (
        cpu_request_millis > 0 AND memory_request_bytes > 0
    ),
    CONSTRAINT chk_lifecycle_event_apply_receipts_route CHECK (
        (event_type IN ('account.runtime.restore_requested', 'account.proxy.change_requested') AND desired_state = 'ready') OR
        (event_type = 'account.runtime.drain_requested' AND desired_state = 'drained') OR
        (event_type = 'account.runtime.destroy_requested' AND desired_state = 'absent')
    ),
    CONSTRAINT chk_lifecycle_event_apply_receipts_timestamps CHECK (event_created_at <= applied_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
