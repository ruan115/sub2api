package main

import (
	"database/sql"
	"fmt"
)

const mysqlSchemaVersion = 1

func (a *app) migrateMySQL() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INT NOT NULL PRIMARY KEY,
			applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS dispatch_strategies (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			description TEXT NOT NULL,
			rpm_limit INT NOT NULL DEFAULT 0,
			tpm_limit BIGINT NOT NULL DEFAULT 0,
			itpm_limit BIGINT NOT NULL DEFAULT 0,
			concurrency_limit INT NOT NULL DEFAULT 0,
			rpm_strategy VARCHAR(32) NOT NULL DEFAULT 'fixed',
			rpm_sticky_buffer INT NOT NULL DEFAULT 0,
			dispatch_mode VARCHAR(32) NOT NULL DEFAULT '',
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			deleted_at DATETIME(3) NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS ` + "`groups`" + ` (
			id VARCHAR(40) NOT NULL PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			description TEXT NOT NULL,
			rate_multiplier DECIMAL(18,8) NOT NULL DEFAULT 1,
			daily_limit_usd DECIMAL(24,12) NULL,
			monthly_limit_usd DECIMAL(24,12) NULL,
			normal_request_mode TINYINT(1) NOT NULL DEFAULT 0,
			claude_code_identity_enabled TINYINT(1) NOT NULL DEFAULT 0,
			stream_hedge_enabled TINYINT(1) NOT NULL DEFAULT 0,
			adaptive_hedge_enabled TINYINT(1) NOT NULL DEFAULT 0,
			rpm_dispatch_enabled TINYINT(1) NOT NULL DEFAULT 1,
			mcp_tool_names_enabled TINYINT(1) NOT NULL DEFAULT 0,
			service_tier_passthrough_enabled TINYINT(1) NOT NULL DEFAULT 0,
			inference_geo_passthrough_enabled TINYINT(1) NOT NULL DEFAULT 0,
			speed_passthrough_enabled TINYINT(1) NOT NULL DEFAULT 0,
			anthropic_beta_passthrough_enabled TINYINT(1) NOT NULL DEFAULT 0,
			reject_anthropic_downgrade_enabled TINYINT(1) NOT NULL DEFAULT 0,
				reject_distillation_enabled TINYINT(1) NOT NULL DEFAULT 0,
				quota_header_masking_enabled TINYINT(1) NOT NULL DEFAULT 0,
				cache_creation_detail_enabled TINYINT(1) NOT NULL DEFAULT 0,
				overload_cooldown_seconds INT NOT NULL DEFAULT 10,
				rate_limit_downweight_enabled TINYINT(1) NOT NULL DEFAULT 1,
				rate_limit_cooling_threshold INT NOT NULL DEFAULT 3,
				rate_limit_wait_seconds INT NOT NULL DEFAULT 120,
				rate_limit_stepped_cooldown_enabled TINYINT(1) NOT NULL DEFAULT 0,
				rate_limit_cooldown_step_seconds INT NOT NULL DEFAULT 30,
				capacity_queue_enabled TINYINT(1) NOT NULL DEFAULT 0,
			capacity_queue_timeout_seconds INT NOT NULL DEFAULT 30,
			strategy_required_enabled TINYINT(1) NOT NULL DEFAULT 0,
			strategy_id BIGINT NULL,
			reserve_pool_enabled TINYINT(1) NOT NULL DEFAULT 0,
			active_reserve_marker TINYINT GENERATED ALWAYS AS (CASE WHEN reserve_pool_enabled = 1 THEN 1 ELSE NULL END) STORED,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY idx_groups_single_reserve_pool (active_reserve_marker),
			CONSTRAINT fk_groups_strategy FOREIGN KEY (strategy_id) REFERENCES dispatch_strategies(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS proxy_pools (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			source_type VARCHAR(16) NOT NULL DEFAULT 'manual',
			api_url TEXT NOT NULL DEFAULT (''),
			api_headers_json LONGTEXT NOT NULL DEFAULT ('{}'),
			default_protocol VARCHAR(16) NOT NULL DEFAULT 'socks5',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			single_use_enabled TINYINT(1) NOT NULL DEFAULT 1,
			system_kind VARCHAR(64) NOT NULL DEFAULT '',
			active_system_kind VARCHAR(64) GENERATED ALWAYS AS (NULLIF(system_kind, '')) STORED,
			last_sync_at DATETIME(3) NULL,
			last_error TEXT NOT NULL DEFAULT (''),
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			deleted_at DATETIME(3) NULL,
			UNIQUE KEY uk_proxy_pools_name (name),
			UNIQUE KEY idx_proxy_pools_system_kind (active_system_kind)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			pool_id BIGINT NOT NULL,
			name VARCHAR(255) NOT NULL,
			protocol VARCHAR(16) NOT NULL,
			host VARCHAR(255) NOT NULL,
			port INT NOT NULL,
			username VARCHAR(512) NOT NULL DEFAULT '',
			password VARCHAR(1024) NOT NULL DEFAULT '',
			identity_hash BINARY(32) GENERATED ALWAYS AS (CASE WHEN deleted_at IS NULL THEN UNHEX(SHA2(CONCAT_WS(CHAR(31), protocol, host, port, username, password), 256)) ELSE NULL END) STORED,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			exit_ip VARCHAR(64) NOT NULL DEFAULT '',
			latency_ms INT NULL,
			last_test_at DATETIME(3) NULL,
			last_error TEXT NOT NULL DEFAULT (''),
			reuse_approved_at DATETIME(3) NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			deleted_at DATETIME(3) NULL,
			UNIQUE KEY idx_proxy_unique (pool_id, identity_hash),
			KEY idx_proxy_pool_status (pool_id, status, deleted_at),
			KEY idx_proxy_identity (protocol, host, port),
			CONSTRAINT fk_proxies_pool FOREIGN KEY (pool_id) REFERENCES proxy_pools(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS users (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			username VARCHAR(191) NOT NULL,
			name VARCHAR(255) NOT NULL DEFAULT '',
			password_hash VARCHAR(255) NOT NULL,
			role VARCHAR(32) NOT NULL DEFAULT 'user',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			allowed_group_ids_json LONGTEXT NOT NULL DEFAULT ('[]'),
			visible_pages_json LONGTEXT NOT NULL DEFAULT ('[]'),
			balance DECIMAL(24,12) NULL,
			rpm_limit INT NOT NULL DEFAULT 0,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			deleted_at DATETIME(3) NULL,
			UNIQUE KEY uk_users_username (username)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			platform VARCHAR(64) NOT NULL DEFAULT 'anthropic',
			auth_type VARCHAR(32) NOT NULL DEFAULT 'oauth',
			credentials_json LONGTEXT NOT NULL DEFAULT ('{}'),
			credential_hint VARCHAR(255) NOT NULL DEFAULT '',
			source_sk_hint VARCHAR(255) NOT NULL DEFAULT '',
			extra_json LONGTEXT NOT NULL DEFAULT ('{}'),
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			schedulable TINYINT(1) NOT NULL DEFAULT 1,
			concurrency INT NOT NULL DEFAULT 10,
			priority INT NOT NULL DEFAULT 50,
			rate_multiplier DECIMAL(18,8) NOT NULL DEFAULT 1,
			notes TEXT NOT NULL DEFAULT (''),
			error_message TEXT NOT NULL DEFAULT (''),
			last_used_at DATETIME(3) NULL,
			expires_at DATETIME(3) NULL,
			rate_limit_reset_at DATETIME(3) NULL,
			rate_limit_window VARCHAR(16) NOT NULL DEFAULT '',
			rate_limit_reason VARCHAR(32) NOT NULL DEFAULT '',
			consecutive_429 INT NOT NULL DEFAULT 0,
			last_429_at DATETIME(3) NULL,
			rate_limit_downweight_until DATETIME(3) NULL,
			quota_refreshed_at DATETIME(3) NULL,
			itpm_remaining BIGINT NULL,
			itpm_reset_at DATETIME(3) NULL,
			itpm_sampled_at DATETIME(3) NULL,
			proxy_pool_id BIGINT NULL,
			proxy_id BIGINT NULL,
			active_proxy_id BIGINT GENERATED ALWAYS AS (CASE WHEN deleted_at IS NULL AND archived_at IS NULL THEN proxy_id ELSE NULL END) STORED,
			auto_proxy TINYINT(1) NOT NULL DEFAULT 0,
			base_rpm INT NOT NULL DEFAULT 0,
			rpm_strategy VARCHAR(32) NOT NULL DEFAULT 'tiered',
			rpm_sticky_buffer INT NOT NULL DEFAULT 0,
			user_msg_queue_mode VARCHAR(32) NOT NULL DEFAULT 'off',
			strategy_id BIGINT NULL,
			auth_status VARCHAR(32) NOT NULL DEFAULT 'unknown',
			auth_error TEXT NOT NULL DEFAULT (''),
			auth_checked_at DATETIME(3) NULL,
			token_expires_at DATETIME(3) NULL,
			quota_5h_utilization DECIMAL(10,4) NOT NULL DEFAULT 0,
			quota_5h_reset_at DATETIME(3) NULL,
			quota_7d_utilization DECIMAL(10,4) NOT NULL DEFAULT 0,
			quota_7d_reset_at DATETIME(3) NULL,
			quota_sampled_at DATETIME(3) NULL,
			subscription_type VARCHAR(64) NOT NULL DEFAULT '',
			rate_limit_tier VARCHAR(128) NOT NULL DEFAULT '',
			account_price DECIMAL(24,12) NOT NULL DEFAULT 0,
			onboarded_at DATETIME(3) NULL,
			reauthorized_at DATETIME(3) NULL,
			reauthorization_count INT NOT NULL DEFAULT 0,
			invalidated_at DATETIME(3) NULL,
			survival_seconds_total BIGINT NOT NULL DEFAULT 0,
			archived_at DATETIME(3) NULL,
			archived_proxy_id BIGINT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			deleted_at DATETIME(3) NULL,
			UNIQUE KEY idx_accounts_proxy_exclusive (active_proxy_id),
			KEY idx_accounts_dispatch (status, schedulable, priority, last_used_at, deleted_at),
			CONSTRAINT fk_accounts_proxy_pool FOREIGN KEY (proxy_pool_id) REFERENCES proxy_pools(id) ON DELETE SET NULL,
			CONSTRAINT fk_accounts_proxy FOREIGN KEY (proxy_id) REFERENCES proxies(id),
			CONSTRAINT fk_accounts_archived_proxy FOREIGN KEY (archived_proxy_id) REFERENCES proxies(id) ON DELETE SET NULL,
			CONSTRAINT fk_accounts_strategy FOREIGN KEY (strategy_id) REFERENCES dispatch_strategies(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_groups (
			account_id BIGINT NOT NULL,
			group_id VARCHAR(40) NOT NULL,
			priority INT NOT NULL DEFAULT 50,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (account_id, group_id),
			KEY idx_account_groups_group (group_id, priority, account_id),
			CONSTRAINT fk_account_groups_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
			CONSTRAINT fk_account_groups_group FOREIGN KEY (group_id) REFERENCES ` + "`groups`" + `(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS purposes (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			` + "`key`" + ` VARCHAR(40) NOT NULL,
			name VARCHAR(255) NOT NULL,
			description TEXT NOT NULL,
			active_group_id VARCHAR(40) NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY uk_purposes_key (` + "`key`" + `),
			CONSTRAINT fk_purposes_group FOREIGN KEY (active_group_id) REFERENCES ` + "`groups`" + `(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS model_prices (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			model VARCHAR(191) NOT NULL,
			input_per_million DECIMAL(24,12) NOT NULL DEFAULT 0,
			output_per_million DECIMAL(24,12) NOT NULL DEFAULT 0,
			cache_creation_per_million DECIMAL(24,12) NOT NULL DEFAULT 0,
			cache_read_per_million DECIMAL(24,12) NOT NULL DEFAULT 0,
			source VARCHAR(16) NOT NULL DEFAULT 'manual',
			source_hash VARCHAR(128) NOT NULL DEFAULT '',
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY uk_model_prices_model (model)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS panel_sessions (
			token_hash CHAR(64) NOT NULL PRIMARY KEY,
			user_id BIGINT NOT NULL,
			expires_at DATETIME(3) NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_sessions_expiry (expires_at),
			CONSTRAINT fk_panel_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			key_hash CHAR(64) NOT NULL,
			key_prefix VARCHAR(64) NOT NULL,
			key_secret VARCHAR(255) NOT NULL DEFAULT '',
			name VARCHAR(255) NOT NULL,
			group_id VARCHAR(40) NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			quota DECIMAL(24,12) NOT NULL DEFAULT 0,
			quota_used DECIMAL(24,12) NOT NULL DEFAULT 0,
			expires_at DATETIME(3) NULL,
			last_used_at DATETIME(3) NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			deleted_at DATETIME(3) NULL,
			UNIQUE KEY uk_api_keys_hash (key_hash),
			KEY idx_api_keys_user (user_id, status, deleted_at),
			CONSTRAINT fk_api_keys_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			CONSTRAINT fk_api_keys_group FOREIGN KEY (group_id) REFERENCES ` + "`groups`" + `(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			request_id VARCHAR(191) NOT NULL,
			client_request_id VARCHAR(191) NOT NULL DEFAULT '',
			trace_id VARCHAR(191) NOT NULL DEFAULT '',
			upstream_request_id VARCHAR(191) NOT NULL DEFAULT '',
			purpose_id BIGINT NULL,
			purpose_key VARCHAR(40) NOT NULL,
			purpose_name VARCHAR(255) NOT NULL,
			group_id VARCHAR(40) NOT NULL,
			account_id BIGINT NOT NULL,
			account_name VARCHAR(255) NOT NULL,
			account_sk_hint VARCHAR(255) NOT NULL DEFAULT '',
			user_id BIGINT NULL,
			api_key_id BIGINT NULL,
			model VARCHAR(191) NOT NULL,
			input_tokens BIGINT NOT NULL DEFAULT 0,
			output_tokens BIGINT NOT NULL DEFAULT 0,
			cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
			cache_read_tokens BIGINT NOT NULL DEFAULT 0,
			input_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			output_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			cache_creation_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			cache_read_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			base_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			billed_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			actual_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			group_rate_multiplier DECIMAL(18,8) NOT NULL DEFAULT 1,
			account_rate_multiplier DECIMAL(18,8) NOT NULL DEFAULT 1,
			stream TINYINT(1) NOT NULL DEFAULT 0,
			duration_ms INT NOT NULL DEFAULT 0,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY uk_usage_request (request_id),
			KEY idx_usage_created (created_at DESC),
			KEY idx_usage_group_created (group_id, created_at DESC),
			KEY idx_usage_account_created (account_id, created_at DESC),
			KEY idx_usage_user_created (user_id, created_at DESC),
			KEY idx_usage_api_key_created (api_key_id, created_at DESC),
			KEY idx_usage_client_request (client_request_id),
			KEY idx_usage_trace (trace_id),
			CONSTRAINT fk_usage_purpose FOREIGN KEY (purpose_id) REFERENCES purposes(id) ON DELETE SET NULL,
			CONSTRAINT fk_usage_group FOREIGN KEY (group_id) REFERENCES ` + "`groups`" + `(id),
			CONSTRAINT fk_usage_account FOREIGN KEY (account_id) REFERENCES accounts(id),
			CONSTRAINT fk_usage_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
			CONSTRAINT fk_usage_api_key FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_usage_totals (
			account_id BIGINT NOT NULL PRIMARY KEY,
			request_count BIGINT NOT NULL DEFAULT 0,
			input_tokens BIGINT NOT NULL DEFAULT 0,
			output_tokens BIGINT NOT NULL DEFAULT 0,
			billed_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			actual_cost DECIMAL(24,12) NOT NULL DEFAULT 0,
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			CONSTRAINT fk_account_usage_totals_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS feature_migrations (
			name VARCHAR(191) NOT NULL PRIMARY KEY,
			applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS user_rpm_events (
			user_id BIGINT NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_user_rpm (user_id, created_at),
			CONSTRAINT fk_user_rpm_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_rpm_events (
			account_id BIGINT NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_account_rpm (account_id, created_at),
			KEY idx_account_rpm_created (created_at, account_id),
			CONSTRAINT fk_account_rpm_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS dispatch_sessions (
			session_hash VARCHAR(191) NOT NULL,
			api_key_id BIGINT NOT NULL,
			account_id BIGINT NOT NULL,
			expires_at DATETIME(3) NOT NULL,
			PRIMARY KEY (session_hash, api_key_id),
			KEY idx_dispatch_sessions_expiry (expires_at),
			CONSTRAINT fk_dispatch_sessions_key FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE,
			CONSTRAINT fk_dispatch_sessions_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_inflight (
			account_id BIGINT NOT NULL PRIMARY KEY,
			requests INT NOT NULL DEFAULT 0,
			CONSTRAINT fk_account_inflight_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_model_cooldowns (
			account_id BIGINT NOT NULL,
			model VARCHAR(191) NOT NULL,
			reset_at DATETIME(3) NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (account_id, model),
			KEY idx_account_model_cooldowns_reset (model, reset_at, account_id),
			CONSTRAINT fk_model_cooldowns_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_rpm_thresholds (
			account_id BIGINT NOT NULL PRIMARY KEY,
			rpm_limit INT NOT NULL,
			reset_at DATETIME(3) NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_account_rpm_thresholds_reset (reset_at, account_id),
			CONSTRAINT fk_rpm_thresholds_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_token_leases (
			account_id BIGINT NOT NULL PRIMARY KEY,
			owner VARCHAR(191) NOT NULL,
			expires_at BIGINT NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_account_token_leases_expiry (expires_at, account_id),
			CONSTRAINT fk_token_leases_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_fingerprints (
			account_id BIGINT NOT NULL PRIMARY KEY,
			fingerprint_json LONGTEXT NOT NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			CONSTRAINT fk_fingerprints_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cache_prefix_events (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			session_hash VARCHAR(191) NOT NULL,
			account_id BIGINT NULL,
			model VARCHAR(191) NOT NULL DEFAULT '',
			prefix_hash VARCHAR(64) NOT NULL,
			tools_hash VARCHAR(64) NOT NULL,
			system_hash VARCHAR(64) NOT NULL,
			changed_segment VARCHAR(32) NOT NULL DEFAULT '',
			previous_prefix_hash VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_cache_prefix_session (session_hash, created_at DESC),
			KEY idx_cache_prefix_created (created_at DESC)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS group_strategy_shares (
			group_id VARCHAR(40) NOT NULL,
			strategy_id BIGINT NOT NULL,
			weight INT NOT NULL DEFAULT 0,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (group_id, strategy_id),
			CONSTRAINT fk_group_strategy_shares_strategy FOREIGN KEY (strategy_id) REFERENCES dispatch_strategies(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS proxy_account_history (
			proxy_id BIGINT NOT NULL,
			account_id BIGINT NOT NULL,
			first_bound_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			last_bound_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			bind_count BIGINT NOT NULL DEFAULT 1,
			PRIMARY KEY (proxy_id, account_id),
			KEY idx_proxy_account_history_account (account_id, last_bound_at DESC),
			CONSTRAINT fk_proxy_history_proxy FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE CASCADE,
			CONSTRAINT fk_proxy_history_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			actor_user_id BIGINT NULL,
			actor_username VARCHAR(191) NOT NULL DEFAULT '',
			actor_role VARCHAR(32) NOT NULL DEFAULT '',
			action VARCHAR(128) NOT NULL,
			method VARCHAR(16) NOT NULL,
			path VARCHAR(512) NOT NULL,
			target_type VARCHAR(128) NOT NULL DEFAULT '',
			target_id VARCHAR(191) NOT NULL DEFAULT '',
			request_body LONGTEXT NOT NULL DEFAULT ('{}'),
			client_ip VARCHAR(64) NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT (''),
			status_code INT NOT NULL,
			duration_ms INT NOT NULL DEFAULT 0,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_audit_created (created_at DESC),
			KEY idx_audit_actor_created (actor_user_id, created_at DESC),
			CONSTRAINT fk_audit_user FOREIGN KEY (actor_user_id) REFERENCES users(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS authorization_logs (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			account_id BIGINT NULL,
			account_name VARCHAR(255) NOT NULL DEFAULT '',
			proxy_id BIGINT NULL,
			proxy_ip VARCHAR(64) NOT NULL DEFAULT '',
			method VARCHAR(64) NOT NULL,
			success TINYINT(1) NOT NULL DEFAULT 0,
			status_message TEXT NOT NULL DEFAULT (''),
			subscription_type VARCHAR(64) NOT NULL DEFAULT '',
			client_ip VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_authorization_created (created_at DESC),
			KEY idx_authorization_account_created (account_id, created_at DESC),
			CONSTRAINT fk_authorization_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE SET NULL,
			CONSTRAINT fk_authorization_proxy FOREIGN KEY (proxy_id) REFERENCES proxies(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS gateway_error_logs (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			request_id VARCHAR(191) NOT NULL DEFAULT '',
			client_request_id VARCHAR(191) NOT NULL DEFAULT '',
			trace_id VARCHAR(191) NOT NULL DEFAULT '',
			upstream_request_id VARCHAR(191) NOT NULL DEFAULT '',
			api_key_id BIGINT NULL,
			user_id BIGINT NULL,
			account_id BIGINT NULL,
			group_id VARCHAR(40) NOT NULL DEFAULT '',
			status_code INT NOT NULL,
			category VARCHAR(128) NOT NULL DEFAULT 'gateway_request',
			method VARCHAR(16) NOT NULL,
			path VARCHAR(512) NOT NULL,
			message TEXT NOT NULL DEFAULT (''),
			client_ip VARCHAR(64) NOT NULL DEFAULT '',
			duration_ms INT NOT NULL DEFAULT 0,
			rpm_snapshot BIGINT NOT NULL DEFAULT -1,
			tpm_snapshot BIGINT NOT NULL DEFAULT -1,
			total_requests BIGINT NOT NULL DEFAULT -1,
			dispatch_diagnostics LONGTEXT NOT NULL DEFAULT (''),
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_gateway_errors_created (created_at DESC),
			KEY idx_gateway_errors_user_created (user_id, created_at DESC),
			KEY idx_gateway_errors_account_created (account_id, created_at DESC),
			KEY idx_gateway_errors_client_request (client_request_id),
			KEY idx_gateway_errors_trace (trace_id),
			CONSTRAINT fk_gateway_errors_key FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE SET NULL,
			CONSTRAINT fk_gateway_errors_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL,
			CONSTRAINT fk_gateway_errors_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE SET NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS account_lifecycle_events (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			account_id BIGINT NOT NULL,
			event_type VARCHAR(32) NOT NULL,
			reason TEXT NOT NULL DEFAULT (''),
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_account_lifecycle_created (event_type, created_at DESC),
			KEY idx_account_lifecycle_account (account_id, created_at DESC),
			CONSTRAINT fk_lifecycle_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS reserve_activation_logs (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			account_id BIGINT NOT NULL,
			source_group_id VARCHAR(40) NOT NULL,
			target_group_id VARCHAR(40) NOT NULL,
			reason VARCHAR(32) NOT NULL,
			requested_model VARCHAR(191) NOT NULL DEFAULT '',
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_reserve_activation_target_created (target_group_id, created_at DESC),
			CONSTRAINT fk_reserve_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
			CONSTRAINT fk_reserve_source_group FOREIGN KEY (source_group_id) REFERENCES ` + "`groups`" + `(id) ON DELETE CASCADE,
			CONSTRAINT fk_reserve_target_group FOREIGN KEY (target_group_id) REFERENCES ` + "`groups`" + `(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS pricing_sync_state (
			id INT NOT NULL PRIMARY KEY,
			remote_url TEXT NOT NULL,
			hash_url TEXT NOT NULL,
			remote_hash VARCHAR(128) NOT NULL DEFAULT '',
			status VARCHAR(32) NOT NULL DEFAULT 'idle',
			model_count INT NOT NULL DEFAULT 0,
			last_synced_at DATETIME(3) NULL,
			last_checked_at DATETIME(3) NULL,
			last_error TEXT NOT NULL DEFAULT ('')
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for index, statement := range statements {
		if _, err := a.db.DB.Exec(statement); err != nil {
			return fmt.Errorf("migrate MySQL schema statement %d: %w", index+1, err)
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"itpm_remaining", "BIGINT NULL"},
		{"itpm_reset_at", "DATETIME(3) NULL"},
		{"itpm_sampled_at", "DATETIME(3) NULL"},
	} {
		if err := ensureMySQLColumn(a.db.DB, "accounts", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := ensureMySQLColumn(a.db.DB, "dispatch_strategies", "itpm_limit", "BIGINT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "rate_limit_downweight_enabled", "TINYINT(1) NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "rate_limit_cooling_threshold", "INT NOT NULL DEFAULT 3"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "rate_limit_wait_seconds", "INT NOT NULL DEFAULT 120"); err != nil {
		return err
	}
	// Through the wrapper, not the raw handle: GROUPS is reserved in MySQL 8.0.2+
	// and only rewriteQuery adds the backticks.
	if _, err := a.db.Exec(`UPDATE groups SET rate_limit_wait_seconds = ? WHERE rate_limit_wait_seconds < ? OR rate_limit_wait_seconds > ?`, defaultRateLimitCooldownSeconds, minRateLimitCooldownSeconds, maxRateLimitCooldownSeconds); err != nil {
		return fmt.Errorf("normalise MySQL group 429 cooldown seconds: %w", err)
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "rate_limit_stepped_cooldown_enabled", "TINYINT(1) NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "rate_limit_cooldown_step_seconds", "INT NOT NULL DEFAULT 30"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE groups SET rate_limit_cooldown_step_seconds = ? WHERE rate_limit_cooldown_step_seconds < 1 OR rate_limit_cooldown_step_seconds > ?`, defaultRateLimitCooldownStepSeconds, maxRateLimitCooldownStepSeconds); err != nil {
		return fmt.Errorf("normalise MySQL group 429 cooldown step seconds: %w", err)
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "quota_header_masking_enabled", "TINYINT(1) NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "groups", "cache_creation_detail_enabled", "TINYINT(1) NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "rate_limit_reason", "VARCHAR(32) NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "consecutive_429", "INT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "last_429_at", "DATETIME(3) NULL"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "rate_limit_downweight_until", "DATETIME(3) NULL"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "quota_refreshed_at", "DATETIME(3) NULL"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "reauthorized_at", "DATETIME(3) NULL"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "accounts", "reauthorization_count", "INT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "proxy_pools", "single_use_enabled", "TINYINT(1) NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureMySQLColumn(a.db.DB, "proxies", "reuse_approved_at", "DATETIME(3) NULL"); err != nil {
		return err
	}
	for _, index := range []struct {
		table, name, columns string
	}{
		{"account_rpm_events", "idx_account_rpm_created", "`created_at`, `account_id`"},
		{"dispatch_sessions", "idx_dispatch_sessions_expiry", "`expires_at`"},
		{"account_model_cooldowns", "idx_account_model_cooldowns_expiry", "`reset_at`, `account_id`, `model`"},
	} {
		if err := ensureMySQLIndex(a.db.DB, index.table, index.name, index.columns); err != nil {
			return err
		}
	}

	seeds := []string{
		`INSERT IGNORE INTO ` + "`groups`" + ` (id, name, description, rate_multiplier) VALUES ('a', 'A 分组', '主业务账号池', 1)`,
		`INSERT IGNORE INTO ` + "`groups`" + ` (id, name, description, rate_multiplier) VALUES ('b', 'B 分组', '备用与隔离账号池', 1)`,
		`INSERT IGNORE INTO purposes (` + "`key`" + `, name, description, active_group_id) VALUES ('default', '默认用途', '未指定用途时使用', 'a')`,
		`INSERT IGNORE INTO model_prices (model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million) VALUES ('*', 3, 15, 3.75, 0.3)`,
		`INSERT IGNORE INTO pricing_sync_state (id, remote_url, hash_url, last_error) VALUES (1,
			'https://raw.githubusercontent.com/Wei-Shaw/model-price-repo/main/model_prices_and_context_window.json',
			'https://raw.githubusercontent.com/Wei-Shaw/model-price-repo/main/model_prices_and_context_window.sha256', '')`,
		`INSERT IGNORE INTO feature_migrations (name) VALUES ('ordinary-user-account-page-v1')`,
		fmt.Sprintf(`INSERT IGNORE INTO schema_migrations (version) VALUES (%d)`, mysqlSchemaVersion),
	}
	for _, statement := range seeds {
		if _, err := a.db.DB.Exec(statement); err != nil {
			return fmt.Errorf("seed MySQL schema: %w", err)
		}
	}
	// Proxy pool seeds, assignment-history backfills and in-flight lease resets
	// are shared with SQLite; newApp calls migrateSharedData after this returns.
	return nil
}

func ensureMySQLColumn(db *sql.DB, table, column, definition string) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table, column).Scan(&count); err != nil {
		return fmt.Errorf("inspect MySQL column %s.%s: %w", table, column, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` %s", table, column, definition)); err != nil {
		return fmt.Errorf("add MySQL column %s.%s: %w", table, column, err)
	}
	return nil
}

func ensureMySQLIndex(db *sql.DB, table, index, columns string) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`, table, index).Scan(&count); err != nil {
		return fmt.Errorf("inspect MySQL index %s.%s: %w", table, index, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE `%s` ADD INDEX `%s` (%s)", table, index, columns)); err != nil {
		return fmt.Errorf("add MySQL index %s.%s: %w", table, index, err)
	}
	return nil
}
