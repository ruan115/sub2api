package main

import (
	"context"
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
	gatewayITPMSettlementTTL          = 61 * time.Second
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

func gatewayEffectiveITPMLimits(strategyLimit int64) (int64, int64) {
	hardLimit := gatewayITPMHardLimit
	if strategyLimit > 0 && strategyLimit < hardLimit {
		hardLimit = strategyLimit
	}
	softLimit := gatewayITPMSoftLimit
	if hardLimit <= softLimit {
		softLimit = hardLimit * 2 / 3
	}
	return softLimit, hardLimit
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
		if inflight > 0 || reserved > 0 {
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
	store.mu.Lock()
	defer store.mu.Unlock()
	lease, ok := store.accounts[accountID][leaseID]
	if !ok {
		return
	}
	lease.tokens = actual
	lease.exclusive = false
	lease.expiresAt = time.Now().Add(gatewayITPMSettlementTTL)
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

func (a *app) reserveGatewayITPM(account *gatewayAccount, demand gatewayDispatchDemand, current, strategyLimit int64, sticky bool, inflight int) (bool, error) {
	if account == nil || demand.EstimatedITPM <= 0 {
		return true, nil
	}
	leaseID, err := secureHex(16)
	if err != nil {
		return false, err
	}
	softLimit, hardLimit := gatewayEffectiveITPMLimits(strategyLimit)
	allowed := false
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		allowed, _, err = a.redis.reserveAccountITPM(ctx, account.ID, leaseID, demand.EstimatedITPM, current, softLimit, hardLimit, sticky, demand.Oversized, inflight, gatewayITPMSmallRequest, gatewayITPMReservationTTL)
		if err != nil {
			return false, err
		}
	} else {
		allowed, _ = a.localITPMReservations.reserve(account.ID, leaseID, demand.EstimatedITPM, current, softLimit, hardLimit, sticky, demand.Oversized, inflight)
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
// one rolling minute; cache_read is deliberately excluded.
func (a *app) settleGatewayITPM(account gatewayAccount, usage tokenUsage) {
	actual := usage.Input + usage.CacheCreation
	if account.ID <= 0 || account.ITPMReservationID == "" || actual <= 0 {
		a.releaseGatewayITPM(account)
		return
	}
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := a.redis.settleAccountITPM(ctx, account.ID, account.ITPMReservationID, actual, gatewayITPMSettlementTTL); err != nil {
			logDatabaseWriteError("settle gateway ITPM reservation", err)
		}
		return
	}
	a.localITPMReservations.settle(account.ID, account.ITPMReservationID, actual)
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
