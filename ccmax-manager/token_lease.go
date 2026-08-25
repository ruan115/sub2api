package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	accountTokenLeaseTTL  = 5 * time.Minute
	accountTokenLeasePoll = 150 * time.Millisecond
	accountTokenLeaseWait = 70 * time.Second
)

var errAccountTokenLeaseLost = errors.New("account token operation was superseded")

func (a *app) acquireAccountTokenLease(ctx context.Context, accountID int64) (string, error) {
	owner, err := secureHex(16)
	if err != nil {
		return "", fmt.Errorf("create account token lease: %w", err)
	}
	waitUntil := time.Now().Add(accountTokenLeaseWait)
	for {
		expiresAt := time.Now().Add(accountTokenLeaseTTL).Unix()
		if a.db.dialect == dialectMySQL {
			result, updateErr := a.db.ExecContext(ctx, `UPDATE account_token_leases
				SET owner = ?, expires_at = ?, updated_at = `+nowSQL+`
				WHERE account_id = ? AND (expires_at <= UNIX_TIMESTAMP(UTC_TIMESTAMP(3)) OR owner = ?)`, owner, expiresAt, accountID, owner)
			if updateErr != nil {
				return "", fmt.Errorf("update MySQL account token lease: %w", updateErr)
			}
			if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 1 {
				return owner, nil
			}
			result, insertErr := a.db.ExecContext(ctx, `INSERT IGNORE INTO account_token_leases (account_id, owner, expires_at) VALUES (?, ?, ?)`, accountID, owner, expiresAt)
			if insertErr != nil {
				return "", fmt.Errorf("insert MySQL account token lease: %w", insertErr)
			}
			if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 1 {
				return owner, nil
			}
		} else {
			result, execErr := a.db.ExecContext(ctx, `INSERT INTO account_token_leases (account_id, owner, expires_at) VALUES (?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at, updated_at = `+nowSQL+`
			WHERE account_token_leases.expires_at <= CAST(strftime('%s','now') AS INTEGER) OR account_token_leases.owner = excluded.owner`, accountID, owner, expiresAt)
			if execErr != nil {
				return "", fmt.Errorf("acquire account token lease: %w", execErr)
			}
			if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 1 {
				return owner, nil
			}
		}
		if time.Now().After(waitUntil) {
			return "", errors.New("another token operation is still in progress for this account")
		}
		timer := time.NewTimer(accountTokenLeasePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("wait for account token lease: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (a *app) releaseAccountTokenLease(accountID int64, owner string) {
	if accountID <= 0 || owner == "" {
		return
	}
	_, err := a.db.Exec(`DELETE FROM account_token_leases WHERE account_id = ? AND owner = ?`, accountID, owner)
	logDatabaseWriteError("release account token lease", err)
}
