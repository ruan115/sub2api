package main

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	gatewayITPMSoftLimit        int64 = 100_000
	gatewayITPMHardLimit        int64 = 150_000
	gatewayITPMLargeRequest     int64 = 100_000
	gatewayITPMSmallRequest     int64 = 10_000
	gatewayITPMReservationTTL         = 30 * time.Minute
	gatewayITPMAutoQueueTimeout       = 5 * time.Second
	// gatewayITPMStickyQueueTimeout bounds how long a pinned warm session waits
	// for its own account's ITPM window to slide before the request gives up
	// the binding and rebuilds its cache on another account.
	gatewayITPMStickyQueueTimeout = 15 * time.Second
)

type gatewayDispatchDemand struct {
	EstimatedITPM int64
	Oversized     bool
}

func estimateGatewayDispatchDemand(body []byte, countTokens bool) gatewayDispatchDemand {
	if countTokens || len(body) == 0 {
		return gatewayDispatchDemand{}
	}
	estimated := 0
	var err error
	// The compatibility tokenizer performs a full protocol conversion first.
	// On multi-hundred-kilobyte prompts that work can take longer than the
	// upstream request itself, while a byte estimate is already sufficient to
	// place the request in the >100k exclusive lane.
	if len(body) <= 256<<10 {
		estimated, err = sub2service.EstimateGrokCountTokens(body)
	}
	if err != nil || estimated <= 0 {
		// Estimation must never become a second request validator. The upstream
		// remains authoritative for request shape and model context limits.
		estimated = (len(body) + 2) / 3
		if estimated < 1 {
			estimated = 1
		}
	}
	value := int64(estimated)
	return gatewayDispatchDemand{EstimatedITPM: value, Oversized: value > gatewayITPMLargeRequest}
}

func gatewayEffectiveITPMLimits(strategyLimit, configuredSoft, configuredHard int64) (int64, int64) {
	hardLimit := configuredHard
	if hardLimit <= 0 {
		hardLimit = gatewayITPMHardLimit
	}
	if strategyLimit > 0 && strategyLimit < hardLimit {
		hardLimit = strategyLimit
	}
	softLimit := configuredSoft
	if softLimit <= 0 {
		softLimit = gatewayITPMSoftLimit
	}
	if hardLimit <= softLimit {
		softLimit = hardLimit * 2 / 3
	}
	return softLimit, hardLimit
}

func normalizeStrategyITPMConfig(enabled bool, window int, soft, hard, strategyLimit int64) (bool, int, int64, int64) {
	if window < minStrategyITPMWindowSeconds || window > maxStrategyITPMWindowSeconds {
		window = defaultStrategyITPMWindowSeconds
	}
	soft, hard = gatewayEffectiveITPMLimits(strategyLimit, soft, hard)
	return enabled, window, soft, hard
}

type accountITPMQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

// loadAccountITPMUsage batches accounts by their strategy window. Keeping the
// cutoff as a bound timestamp works identically on SQLite and MySQL and avoids
// a correlated usage_logs scan for every candidate on every dispatch attempt.
func loadAccountITPMUsage(queryer accountITPMQueryer, accountWindows map[int64]int) (map[int64]int64, error) {
	result := map[int64]int64{}
	byWindow := map[int][]int64{}
	for accountID, window := range accountWindows {
		if accountID <= 0 {
			continue
		}
		if window < minStrategyITPMWindowSeconds || window > maxStrategyITPMWindowSeconds {
			window = defaultStrategyITPMWindowSeconds
		}
		byWindow[window] = append(byWindow[window], accountID)
	}
	const queryChunkSize = 400
	for window, accountIDs := range byWindow {
		for offset := 0; offset < len(accountIDs); offset += queryChunkSize {
			end := min(offset+queryChunkSize, len(accountIDs))
			chunk := accountIDs[offset:end]
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			args := make([]any, 0, len(chunk)+1)
			args = append(args, time.Now().UTC().Add(-time.Duration(window)*time.Second).Format(time.RFC3339Nano))
			for _, accountID := range chunk {
				args = append(args, accountID)
			}
			rows, err := queryer.Query(`SELECT account_id, COALESCE(SUM(input_tokens + cache_creation_tokens), 0)
				FROM usage_logs WHERE created_at >= ? AND account_id IN (`+placeholders+`) GROUP BY account_id`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var accountID, tokens int64
				if err := rows.Scan(&accountID, &tokens); err != nil {
					rows.Close()
					return nil, err
				}
				result[accountID] = tokens
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}
	return result, nil
}

type localITPMReservation struct {
	tokens    int64
	exclusive bool
	expiresAt time.Time
}

type localITPMReservationStore struct {
	mu       sync.Mutex
	accounts map[int64]map[string]localITPMReservation
}

type accountITPMReservationStatus struct {
	Tokens    int64
	Exclusive bool
}

func (store *localITPMReservationStore) reserve(accountID int64, leaseID string, estimated, current, softLimit, hardLimit int64, sticky, exclusive bool, inflight int) (bool, int64) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.accounts == nil {
		store.accounts = map[int64]map[string]localITPMReservation{}
	}
	now := time.Now()
	leases := store.accounts[accountID]
	if leases == nil {
		leases = map[string]localITPMReservation{}
		store.accounts[accountID] = leases
	}
	reserved := int64(0)
	hasExclusive := false
	for id, lease := range leases {
		if !lease.expiresAt.After(now) {
			delete(leases, id)
			continue
		}
		reserved += lease.tokens
		hasExclusive = hasExclusive || lease.exclusive
	}
	if hasExclusive {
		return false, reserved
	}
	if exclusive {
		if inflight > 0 || reserved > 0 || (hardLimit > 0 && current >= hardLimit) {
			return false, reserved
		}
	} else {
		projected := current + reserved + estimated
		if hardLimit > 0 && (current+reserved >= hardLimit || projected > hardLimit) {
			return false, reserved
		}
		if softLimit > 0 && projected > softLimit && !sticky && estimated > gatewayITPMSmallRequest {
			return false, reserved
		}
	}
	leases[leaseID] = localITPMReservation{tokens: estimated, exclusive: exclusive, expiresAt: now.Add(gatewayITPMReservationTTL)}
	return true, reserved + estimated
}

func (store *localITPMReservationStore) release(accountID int64, leaseID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.accounts == nil {
		return
	}
	leases := store.accounts[accountID]
	delete(leases, leaseID)
	if len(leases) == 0 {
		delete(store.accounts, accountID)
	}
}

func (store *localITPMReservationStore) settle(accountID int64, leaseID string, actual int64) {
	store.settleFor(accountID, leaseID, actual, time.Duration(defaultStrategyITPMWindowSeconds+1)*time.Second)
}

func (store *localITPMReservationStore) settleFor(accountID int64, leaseID string, actual int64, ttl time.Duration) {
	store.mu.Lock()
	defer store.mu.Unlock()
	lease, ok := store.accounts[accountID][leaseID]
	if !ok {
		return
	}
	lease.tokens = actual
	lease.exclusive = false
	lease.expiresAt = time.Now().Add(ttl)
	store.accounts[accountID][leaseID] = lease
}

func (store *localITPMReservationStore) renew(accountID int64, leaseID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	lease, ok := store.accounts[accountID][leaseID]
	if !ok {
		return
	}
	lease.expiresAt = time.Now().Add(gatewayITPMReservationTTL)
	store.accounts[accountID][leaseID] = lease
}

func (store *localITPMReservationStore) statuses(accountIDs []int64) map[int64]accountITPMReservationStatus {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := map[int64]accountITPMReservationStatus{}
	now := time.Now()
	for _, accountID := range accountIDs {
		leases := store.accounts[accountID]
		status := accountITPMReservationStatus{}
		for leaseID, lease := range leases {
			if !lease.expiresAt.After(now) {
				delete(leases, leaseID)
				continue
			}
			status.Tokens += lease.tokens
			status.Exclusive = status.Exclusive || lease.exclusive
		}
		if len(leases) == 0 {
			delete(store.accounts, accountID)
		}
		if status.Tokens > 0 || status.Exclusive {
			result[accountID] = status
		}
	}
	return result
}

func (a *app) accountITPMReservationStatuses(accountIDs []int64) (map[int64]accountITPMReservationStatus, error) {
	if len(accountIDs) == 0 {
		return map[int64]accountITPMReservationStatus{}, nil
	}
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return a.redis.accountITPMReservationStatuses(ctx, accountIDs)
	}
	return a.localITPMReservations.statuses(accountIDs), nil
}

func (a *app) reserveGatewayITPM(account *gatewayAccount, demand gatewayDispatchDemand, current int64, sticky bool, inflight int) (bool, error) {
	if account == nil || !account.ITPMProtection || demand.EstimatedITPM <= 0 {
		return true, nil
	}
	leaseID, err := secureHex(16)
	if err != nil {
		return false, err
	}
	allowed := false
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		allowed, _, err = a.redis.reserveAccountITPM(ctx, account.ID, leaseID, demand.EstimatedITPM, current, account.ITPMSoftLimit, account.ITPMHardLimit, sticky, demand.Oversized, inflight, gatewayITPMSmallRequest, gatewayITPMReservationTTL)
		if err != nil {
			return false, err
		}
	} else {
		allowed, _ = a.localITPMReservations.reserve(account.ID, leaseID, demand.EstimatedITPM, current, account.ITPMSoftLimit, account.ITPMHardLimit, sticky, demand.Oversized, inflight)
	}
	if !allowed {
		return false, nil
	}
	account.ITPMReservationID = leaseID
	account.EstimatedITPM = demand.EstimatedITPM
	account.ITPMExclusive = demand.Oversized
	return true, nil
}

func (a *app) releaseGatewayITPM(account gatewayAccount) {
	if account.ID <= 0 || account.ITPMReservationID == "" {
		return
	}
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := a.redis.releaseAccountITPM(ctx, account.ID, account.ITPMReservationID); err != nil {
			logDatabaseWriteError("release gateway ITPM reservation", err)
		}
		return
	}
	a.localITPMReservations.release(account.ID, account.ITPMReservationID)
}

// settleGatewayITPM is the safety net for a successful upstream response whose
// usage row could not be committed. Actual uncached input remains visible for
// the strategy's rolling window; cache_read is deliberately excluded.
func (a *app) settleGatewayITPM(account gatewayAccount, usage tokenUsage) {
	actual := usage.Input + usage.CacheCreation
	if account.ID <= 0 || account.ITPMReservationID == "" || actual <= 0 {
		a.releaseGatewayITPM(account)
		return
	}
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := a.redis.settleAccountITPM(ctx, account.ID, account.ITPMReservationID, actual, account.itpmSettlementTTL()); err != nil {
			logDatabaseWriteError("settle gateway ITPM reservation", err)
		}
		return
	}
	a.localITPMReservations.settleFor(account.ID, account.ITPMReservationID, actual, account.itpmSettlementTTL())
}

func (account gatewayAccount) itpmSettlementTTL() time.Duration {
	window := account.ITPMWindowSeconds
	if window < minStrategyITPMWindowSeconds || window > maxStrategyITPMWindowSeconds {
		window = defaultStrategyITPMWindowSeconds
	}
	return time.Duration(window+1) * time.Second
}

func (a *app) keepGatewayITPMReservation(account gatewayAccount) func() {
	if account.ID <= 0 || account.ITPMReservationID == "" {
		return func() {}
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(gatewayITPMReservationTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if a.redis != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					err := a.redis.renewAccountITPM(ctx, account.ID, account.ITPMReservationID, gatewayITPMReservationTTL)
					cancel()
					if err != nil {
						logDatabaseWriteError("renew gateway ITPM reservation", err)
					}
				} else {
					a.localITPMReservations.renew(account.ID, account.ITPMReservationID)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

func gatewayITPMCapacityBlocked(err error) bool {
	diagnostics, ok := gatewayCapacityDiagnosticsFromError(err)
	return ok && (diagnostics.ITPMBlocked > 0 || diagnostics.ITPMReservationBlocked > 0)
}
