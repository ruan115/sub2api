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
		result, execErr := a.db.ExecContext(ctx, `INSERT INTO account_token_leases (account_id, owner, expires_at) VALUES (?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at, updated_at = `+nowSQL+`
			WHERE account_token_leases.expires_at <= CAST(strftime('%s','now') AS INTEGER) OR account_token_leases.owner = excluded.owner`, accountID, owner, expiresAt)
		if execErr != nil {
			return "", fmt.Errorf("acquire account token lease: %w", execErr)
		}
		if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 1 {
			return owner, nil
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
	_, _ = a.db.Exec(`DELETE FROM account_token_leases WHERE account_id = ? AND owner = ?`, accountID, owner)
}
