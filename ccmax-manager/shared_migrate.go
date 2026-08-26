package main

import (
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// migrateSharedData runs the data migrations that both dialects need. Schema
// DDL stays split — SQLite grows columns through addColumnIfMissing and
// migrateMySQL declares them up front — but the backfills below only read and
// write rows, so a single implementation serves both.
//
// Every statement must go through a.db (the *database wrapper) rather than
// a.db.DB, because only the wrapper applies rewriteQuery. Writing SQLite
// flavoured SQL here is intentional: rewriteQuery translates it for MySQL.
//
// Ordering matters. Later statements read columns that earlier ones populate,
// and the proxy history backfills must precede migrateDeadProxyAssignments so
// quarantine decisions see the complete binding record.
//
// Every statement must also be idempotent: this runs on each startup.
func (a *app) migrateSharedData() error {
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO proxy_pools (id, name, source_type, default_protocol) VALUES (1, 'default', 'manual', 'socks5')`); err != nil {
		return fmt.Errorf("seed default proxy pool: %w", err)
	}
	if err := a.ensureDeadProxyPoolExists(); err != nil {
		return err
	}
	// Proxy assignment history is what makes a single-use address stay burned
	// after its account is deleted or archived. SQLite maintains it through
	// triggers and MySQL through recordProxyAssignment, but rows that predate
	// either mechanism still need backfilling.
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
		SELECT proxy_id, id, created_at, updated_at, 1 FROM accounts WHERE proxy_id IS NOT NULL`); err != nil {
		return fmt.Errorf("backfill proxy assignment history: %w", err)
	}
	// Archiving moves proxy_id into archived_proxy_id, so the live backfill
	// above cannot see archived bindings. Without this an archived account's
	// address looks untouched and single-use pools hand it out again.
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
		SELECT archived_proxy_id, id, created_at, updated_at, 1 FROM accounts WHERE archived_proxy_id IS NOT NULL`); err != nil {
		return fmt.Errorf("backfill archived proxy assignment history: %w", err)
	}
	if err := a.migrateDeadProxyAssignments(); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE accounts SET auth_status = 'valid' WHERE auth_status = 'unknown' AND credentials_json != '{}'`); err != nil {
		return fmt.Errorf("initialize account authorization state: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET onboarded_at = COALESCE(auth_checked_at, created_at) WHERE onboarded_at IS NULL AND auth_status = 'valid'`); err != nil {
		return fmt.Errorf("initialize account onboarding time: %w", err)
	}
	if err := a.reclassifyRevokedOAuthAccounts(); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE accounts SET invalidated_at = NULL WHERE onboarded_at IS NULL AND status != 'error'`); err != nil {
		return fmt.Errorf("clear invalid pending-account timestamps: %w", err)
	}
	if _, err := a.db.Exec(`DELETE FROM account_lifecycle_events WHERE event_type = 'invalidated' AND account_id IN (SELECT id FROM accounts WHERE onboarded_at IS NULL AND status != 'error')`); err != nil {
		return fmt.Errorf("clear invalid pending-account events: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET invalidated_at = COALESCE(auth_checked_at, updated_at) WHERE invalidated_at IS NULL AND (status = 'error' OR (auth_status = 'reauth_required' AND onboarded_at IS NOT NULL))`); err != nil {
		return fmt.Errorf("initialize account invalidation time: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET survival_seconds_total = MAX(0, CAST(strftime('%s', invalidated_at) AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER)) WHERE survival_seconds_total = 0 AND onboarded_at IS NOT NULL AND invalidated_at IS NOT NULL`); err != nil {
		return fmt.Errorf("initialize account accumulated survival: %w", err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, created_at)
		SELECT id, 'onboarded', onboarded_at FROM accounts a WHERE onboarded_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM account_lifecycle_events e WHERE e.account_id = a.id AND e.event_type = 'onboarded')`); err != nil {
		return fmt.Errorf("initialize account onboarding events: %w", err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, reason, created_at)
		SELECT id, 'invalidated', COALESCE(NULLIF(auth_error, ''), error_message), invalidated_at FROM accounts a WHERE invalidated_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM account_lifecycle_events e WHERE e.account_id = a.id AND e.event_type = 'invalidated')`); err != nil {
		return fmt.Errorf("initialize account invalidation events: %w", err)
	}
	// Events written before the reason column existed can still be attributed
	// while the account remains invalidated and its auth_error is intact.
	if _, err := a.db.Exec(`UPDATE account_lifecycle_events SET reason = COALESCE((
			SELECT NULLIF(a.auth_error, '') FROM accounts a WHERE a.id = account_lifecycle_events.account_id AND a.invalidated_at IS NOT NULL
		), '') WHERE event_type = 'invalidated' AND reason = ''`); err != nil {
		return fmt.Errorf("backfill account invalidation reasons: %w", err)
	}
	if err := a.migrateReauthorizationStats(); err != nil {
		return err
	}
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO account_usage_totals
		(account_id, request_count, input_tokens, output_tokens, billed_cost, actual_cost, updated_at)
		SELECT account_id, COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0), MAX(created_at)
		FROM usage_logs GROUP BY account_id`); err != nil {
		return fmt.Errorf("backfill account usage totals: %w", err)
	}
	if err := a.backfillAccountSubscriptions(); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE users SET visible_pages_json = CASE role WHEN 'admin' THEN '["overview","accounts","dead","onboarding","daily","authorization","errors","proxies","access","pricing","billing","audit"]' WHEN 'readonly_admin' THEN '["overview","accounts","dead","daily","authorization","errors","proxies","pricing","billing","audit"]' ELSE '["accounts","access"]' END WHERE visible_pages_json = '[]'`); err != nil {
		return fmt.Errorf("initialize visible pages: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE users SET visible_pages_json = '["overview","accounts","dead","daily","authorization","errors","proxies","pricing","billing","audit"]' WHERE role = 'readonly_admin' AND visible_pages_json = '["overview","accounts","dead","daily","authorization","proxies","pricing","billing","audit"]'`); err != nil {
		return fmt.Errorf("add error page to default read-only permissions: %w", err)
	}
	if err := a.ensureAdminUser(); err != nil {
		return err
	}
	if err := a.migrateGroupStrategyShares(); err != nil {
		return err
	}
	if err := a.migrateCachePrefixAudit(); err != nil {
		return err
	}
	// In-flight counters are process leases. A previous process cannot release
	// them after a crash or restart, so never carry them into this process.
	if _, err := a.db.Exec(`DELETE FROM account_inflight`); err != nil {
		return fmt.Errorf("reset stale account in-flight leases: %w", err)
	}
	return nil
}

func (a *app) migrateReauthorizationStats() error {
	if _, err := a.db.Exec(`UPDATE accounts SET
		reauthorization_count = COALESCE((SELECT CASE WHEN COUNT(*) > 1 THEN COUNT(*) - 1 ELSE 0 END
			FROM account_lifecycle_events e WHERE e.account_id = accounts.id AND e.event_type = 'onboarded'), 0),
		reauthorized_at = (SELECT MAX(e.created_at) FROM account_lifecycle_events e
			WHERE e.account_id = accounts.id AND e.event_type = 'onboarded'
			AND e.created_at > (SELECT MIN(first_event.created_at) FROM account_lifecycle_events first_event
				WHERE first_event.account_id = accounts.id AND first_event.event_type = 'onboarded'))
		WHERE reauthorization_count = 0 AND reauthorized_at IS NULL
		AND (SELECT COUNT(*) FROM account_lifecycle_events existing_events
			WHERE existing_events.account_id = accounts.id AND existing_events.event_type = 'onboarded') > 1`); err != nil {
		return fmt.Errorf("backfill reauthorization stats: %w", err)
	}
	return nil
}

// ensureDeadProxyPoolExists creates the quarantine pool that holds addresses
// released by archived accounts. proxyNotQuarantinedPredicate matches against
// it, so a missing pool silently disables single-use enforcement entirely.
func (a *app) ensureDeadProxyPoolExists() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := ensureDeadProxyPool(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create dead proxy pool: %w", err)
	}
	return nil
}

func (a *app) ensureAdminUser() error {
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(a.adminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	visible, _ := json.Marshal(defaultVisiblePages("admin"))
	if _, err := a.db.Exec(`INSERT INTO users (username, name, password_hash, role, allowed_group_ids_json, visible_pages_json) VALUES (?, '系统管理员', ?, 'admin', '["a","b"]', ?)`, a.adminUser, string(hash), string(visible)); err != nil {
		return fmt.Errorf("seed administrator: %w", err)
	}
	return nil
}
