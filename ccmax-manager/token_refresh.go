package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	backgroundTokenRefreshBefore = 30 * time.Minute
	gatewayTokenRefreshBefore    = 3 * time.Minute
)

func (a *app) startTokenRefreshScheduler() func() {
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		runTokenRefreshCycle(ctx, a.refreshExpiringTokens)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runTokenRefreshCycle(ctx, a.refreshExpiringTokens)
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wait.Wait()
		})
	}
}

func runTokenRefreshCycle(parent context.Context, refresh func(context.Context)) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	refresh(ctx)
}

func (a *app) refreshExpiringTokens(parent context.Context) {
	accounts, err := a.expiringGatewayAccounts(backgroundTokenRefreshBefore)
	if err != nil {
		log.Printf("token refresh candidates: %v", err)
		return
	}
	if len(accounts) == 0 {
		return
	}

	jobs := make(chan gatewayAccount)
	var wait sync.WaitGroup
	var providerMu sync.Mutex
	consecutiveFailures := 0
	providerTripped := false
	providerAvailable := func() bool {
		providerMu.Lock()
		defer providerMu.Unlock()
		return !providerTripped
	}
	recordProviderResult := func(refreshErr error) {
		providerMu.Lock()
		defer providerMu.Unlock()
		if refreshErr == nil {
			consecutiveFailures = 0
			return
		}
		if errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) {
			return
		}
		var permanent *claudeRefreshError
		if errors.As(refreshErr, &permanent) && permanent.permanent() {
			consecutiveFailures = 0
			return
		}
		consecutiveFailures++
		if consecutiveFailures >= 3 {
			providerTripped = true
		}
	}
	workers := 4
	if workers > len(accounts) {
		workers = len(accounts)
	}
	rate := time.NewTicker(500 * time.Millisecond)
	defer rate.Stop()
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for account := range jobs {
				if !providerAvailable() {
					continue
				}
				var refreshErr error
				for attempt := 1; attempt <= 3; attempt++ {
					select {
					case <-parent.Done():
						return
					case <-rate.C:
					}
					ctx, cancel := context.WithTimeout(parent, 15*time.Second)
					_, refreshErr = a.refreshGatewayAccountToken(ctx, account, true)
					cancel()
					if refreshErr == nil {
						break
					}
					var permanent *claudeRefreshError
					if errors.As(refreshErr, &permanent) && permanent.permanent() {
						break
					}
					if attempt < 3 {
						delay := backgroundTokenRefreshBackoff(account.ID, attempt)
						select {
						case <-parent.Done():
							return
						case <-time.After(delay):
						}
					}
				}
				recordProviderResult(refreshErr)
				if refreshErr != nil {
					until := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
					reason := "token refresh retry exhausted: " + refreshErr.Error()
					_, _ = a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, auth_error = ?, auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ? AND auth_status != 'reauth_required'`, until, reason, account.ID)
					log.Printf("token refresh account %d: %v", account.ID, refreshErr)
				}
			}
		}()
	}
	for _, account := range accounts {
		select {
		case jobs <- account:
		case <-parent.Done():
			close(jobs)
			wait.Wait()
			return
		}
	}
	close(jobs)
	wait.Wait()
}

func backgroundTokenRefreshBackoff(accountID int64, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := 2 * time.Second * time.Duration(1<<(attempt-1))
	jitterPercent := int64(75) + (accountID+int64(attempt*17))%51
	return base * time.Duration(jitterPercent) / 100
}

func (a *app) expiringGatewayAccounts(before time.Duration) ([]gatewayAccount, error) {
	rows, err := a.db.Query(`SELECT a.id, a.name, a.auth_type, a.credentials_json, a.extra_json, a.concurrency, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode, a.proxy_id, a.auth_error, a.rate_limit_reset_at
		FROM accounts a
		WHERE a.deleted_at IS NULL AND a.status = 'active' AND a.schedulable = 1 AND a.auth_status != 'reauth_required'
		AND a.auth_type IN ('oauth', 'setup_token') AND a.proxy_id IS NOT NULL
		AND EXISTS (SELECT 1 FROM proxies p WHERE p.id = a.proxy_id AND p.status = 'active' AND p.deleted_at IS NULL)
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]gatewayAccount, 0)
	for rows.Next() {
		var item gatewayAccount
		var authError string
		var resetAt sql.NullString
		if err := rows.Scan(&item.ID, &item.Name, &item.AuthType, &item.CredentialsJSON, &item.ExtraJSON, &item.Concurrency, &item.BaseRPM, &item.RPMStrategy, &item.StickyBuffer, &item.UserMsgQueueMode, &item.ProxyID, &authError, &resetAt); err != nil {
			return nil, err
		}
		if strings.HasPrefix(authError, "token refresh retry exhausted:") && resetAt.Valid {
			if reset, parseErr := time.Parse(time.RFC3339Nano, resetAt.String); parseErr == nil && reset.After(time.Now()) {
				continue
			}
		}
		credentials := decodeObject(item.CredentialsJSON)
		refreshToken, _ := credentials["refresh_token"].(string)
		expiresAt := int64FromAny(credentials["expires_at"])
		if strings.TrimSpace(refreshToken) == "" || expiresAt <= 0 {
			continue
		}
		if time.Until(time.Unix(expiresAt, 0)) <= before {
			result = append(result, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
