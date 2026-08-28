package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

const sub2FingerprintExtraKey = "sub2_fingerprint"

const defaultAccountNodeRuntimeVersion = "v24.3.0"

func accountCompatibilityFingerprint(account gatewayAccount) *sub2service.Fingerprint {
	return account.Fingerprint
}

// normalizeAccountFingerprintForTLSProfile keeps the HTTP identity coherent
// with the fixed Node.js 24 TLS profile. ClientID and the per-account platform
// fields remain stable; only fields that would contradict the transport are
// normalized.
func normalizeAccountFingerprintForTLSProfile(fingerprint *sub2service.Fingerprint, profile string) (*sub2service.Fingerprint, bool) {
	if fingerprint == nil || normalizeAccountTLSProfile(profile) != defaultAccountTLSProfile {
		return fingerprint, false
	}
	next := *fingerprint
	changed := false
	if next.StainlessLang != "js" {
		next.StainlessLang = "js"
		changed = true
	}
	if next.StainlessRuntime != "node" {
		next.StainlessRuntime = "node"
		changed = true
	}
	if next.StainlessRuntimeVersion != defaultAccountNodeRuntimeVersion {
		next.StainlessRuntimeVersion = defaultAccountNodeRuntimeVersion
		changed = true
	}
	if !changed {
		return fingerprint, false
	}
	return &next, true
}

func decodeCompatibilityFingerprint(raw any) *sub2service.Fingerprint {
	if raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var fingerprint sub2service.Fingerprint
	if json.Unmarshal(encoded, &fingerprint) != nil || strings.TrimSpace(fingerprint.ClientID) == "" {
		return nil
	}
	return &fingerprint
}

func (a *app) ensureGatewayAccountFingerprint(account gatewayAccount, headers http.Header) (gatewayAccount, error) {
	account.TLSProfile = normalizeAccountTLSProfile(account.TLSProfile)
	if !gatewayAccountUsesOAuth(account) {
		return account, nil
	}

	lockValue, _ := a.tokenLocks.LoadOrStore(account.ID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	var latestExtra string
	if err := a.db.QueryRow(`SELECT extra_json FROM accounts WHERE id = ? AND deleted_at IS NULL`, account.ID).Scan(&latestExtra); err != nil {
		return account, err
	}
	account.ExtraJSON = latestExtra
	var storedJSON, storedTLSProfile string
	rowErr := a.db.QueryRow(`SELECT fingerprint_json, tls_profile FROM account_fingerprints WHERE account_id = ?`, account.ID).Scan(&storedJSON, &storedTLSProfile)
	var existing *sub2service.Fingerprint
	if rowErr == nil {
		account.TLSProfile = normalizeAccountTLSProfile(storedTLSProfile)
		var raw any
		if json.Unmarshal([]byte(storedJSON), &raw) == nil {
			existing = decodeCompatibilityFingerprint(raw)
		}
	} else if rowErr != sql.ErrNoRows {
		return account, rowErr
	}

	extra := decodeObject(latestExtra)
	legacy, hasLegacy := extra[sub2FingerprintExtraKey]
	if existing == nil && hasLegacy {
		existing = decodeCompatibilityFingerprint(legacy)
	}
	resolved, changed := sub2service.ResolveCCMaxCompatibilityFingerprint(headers, existing)
	resolved, normalized := normalizeAccountFingerprintForTLSProfile(resolved, account.TLSProfile)
	if !changed && !normalized && existing != nil && !hasLegacy {
		account.Fingerprint = existing
		return account, nil
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return account, err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return account, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO account_fingerprints (account_id, fingerprint_json, tls_profile) VALUES (?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET fingerprint_json = excluded.fingerprint_json, updated_at = `+nowSQL,
		account.ID, string(encoded), account.TLSProfile); err != nil {
		return account, err
	}
	if hasLegacy {
		delete(extra, sub2FingerprintExtraKey)
		extraJSON, marshalErr := json.Marshal(extra)
		if marshalErr != nil {
			return account, marshalErr
		}
		if _, err := tx.Exec(`UPDATE accounts SET extra_json = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, string(extraJSON), account.ID); err != nil {
			return account, err
		}
		account.ExtraJSON = string(extraJSON)
	}
	if err := tx.Commit(); err != nil {
		return account, err
	}
	account.Fingerprint = resolved
	return account, nil
}

func seedAccountFingerprint(tx *databaseTx, accountID int64, headers http.Header) error {
	resolved, _ := sub2service.ResolveCCMaxCompatibilityFingerprint(headers, nil)
	resolved, _ = normalizeAccountFingerprintForTLSProfile(resolved, defaultAccountTLSProfile)
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO account_fingerprints (account_id, fingerprint_json, tls_profile) VALUES (?, ?, ?)`,
		accountID, string(encoded), defaultAccountTLSProfile)
	return err
}

func (a *app) backfillAccountFingerprints() error {
	rows, err := a.db.Query(`SELECT a.id FROM accounts a LEFT JOIN account_fingerprints f ON f.account_id = a.id
		WHERE a.deleted_at IS NULL AND f.account_id IS NULL ORDER BY a.id`)
	if err != nil {
		return err
	}
	var accountIDs []int64
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			rows.Close()
			return err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(accountIDs) == 0 {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, accountID := range accountIDs {
		if err := seedAccountFingerprint(tx, accountID, nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) normalizeStoredAccountFingerprints() error {
	rows, err := a.db.Query(`SELECT account_id, fingerprint_json, tls_profile FROM account_fingerprints ORDER BY account_id`)
	if err != nil {
		return err
	}
	type fingerprintUpdate struct {
		accountID int64
		encoded   string
		profile   string
	}
	updates := make([]fingerprintUpdate, 0)
	for rows.Next() {
		var accountID int64
		var raw, profile string
		if err := rows.Scan(&accountID, &raw, &profile); err != nil {
			rows.Close()
			return err
		}
		normalizedProfile := normalizeAccountTLSProfile(profile)
		var fingerprint sub2service.Fingerprint
		repaired := false
		if err := json.Unmarshal([]byte(raw), &fingerprint); err != nil || strings.TrimSpace(fingerprint.ClientID) == "" {
			resolved, _ := sub2service.ResolveCCMaxCompatibilityFingerprint(nil, nil)
			fingerprint = *resolved
			repaired = true
		}
		resolved, changed := normalizeAccountFingerprintForTLSProfile(&fingerprint, normalizedProfile)
		if !repaired && !changed && normalizedProfile == profile {
			continue
		}
		encoded, err := json.Marshal(resolved)
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, fingerprintUpdate{accountID: accountID, encoded: string(encoded), profile: normalizedProfile})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, update := range updates {
		if _, err := tx.Exec(`UPDATE account_fingerprints SET fingerprint_json = ?, tls_profile = ?, updated_at = `+nowSQL+` WHERE account_id = ?`, update.encoded, update.profile, update.accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
