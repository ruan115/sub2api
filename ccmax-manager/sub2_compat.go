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

func accountCompatibilityFingerprint(account gatewayAccount) *sub2service.Fingerprint {
	return account.Fingerprint
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
	var storedJSON string
	rowErr := a.db.QueryRow(`SELECT fingerprint_json FROM account_fingerprints WHERE account_id = ?`, account.ID).Scan(&storedJSON)
	var existing *sub2service.Fingerprint
	if rowErr == nil {
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
	if !changed && existing != nil && !hasLegacy {
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
	if _, err := tx.Exec(`INSERT INTO account_fingerprints (account_id, fingerprint_json) VALUES (?, ?)
		ON CONFLICT(account_id) DO UPDATE SET fingerprint_json = excluded.fingerprint_json, updated_at = `+nowSQL, account.ID, string(encoded)); err != nil {
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
