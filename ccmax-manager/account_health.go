package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type accountHealthController struct {
	mu sync.Mutex
}

type accountHealthResult struct {
	Checked   int      `json:"checked"`
	Healthy   int      `json:"healthy"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"`
	Errors    []string `json:"errors,omitempty"`
	CheckedAt string   `json:"checked_at"`
}

func newAccountHealthController() *accountHealthController {
	return &accountHealthController{}
}

func (a *app) startAccountHealthScheduler() func() {
	enabled := strings.TrimSpace(os.Getenv("CCMAX_ACCOUNT_HEALTH_ENABLED"))
	// Sub2API does not actively probe every account on a timer. Keep this
	// standalone extension opt-in so the compatibility lifecycle stays intact.
	if enabled != "1" && !strings.EqualFold(enabled, "true") {
		return func() {}
	}
	minutes := 5
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CCMAX_ACCOUNT_HEALTH_MINUTES"))); err == nil && value > 0 {
		minutes = value
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		if result, err := a.refreshAccountHealth(ctx, nil, false); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("account health initial check: %v", err)
		} else {
			log.Printf("account health initial check: checked=%d healthy=%d failed=%d skipped=%d", result.Checked, result.Healthy, result.Failed, result.Skipped)
		}
		ticker := time.NewTicker(time.Duration(minutes) * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				result, err := a.refreshAccountHealth(ctx, nil, false)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					log.Printf("account health scheduled check: %v", err)
					continue
				}
				log.Printf("account health scheduled check: checked=%d healthy=%d failed=%d skipped=%d", result.Checked, result.Healthy, result.Failed, result.Skipped)
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

func (a *app) handleAccountHealthRefresh(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []int64 `json:"ids"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &input) {
		return
	}
	result, err := a.refreshAccountHealth(r.Context(), input.IDs, true)
	if err != nil {
		if errors.Is(err, errAccountHealthRunning) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

var errAccountHealthRunning = errors.New("账户存活检测正在运行")

func (a *app) refreshAccountHealth(parent context.Context, requestedIDs []int64, manual bool) (accountHealthResult, error) {
	if !a.accountHealth.mu.TryLock() {
		return accountHealthResult{}, errAccountHealthRunning
	}
	defer a.accountHealth.mu.Unlock()

	ids, skipped, err := a.accountHealthIDs(requestedIDs, manual)
	if err != nil {
		return accountHealthResult{}, err
	}
	result := accountHealthResult{Skipped: skipped, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if len(ids) == 0 {
		return result, nil
	}

	workers := 2
	if value, parseErr := strconv.Atoi(strings.TrimSpace(os.Getenv("CCMAX_ACCOUNT_HEALTH_CONCURRENCY"))); parseErr == nil && value > 0 && value <= 10 {
		workers = value
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	type checkResult struct {
		id  int64
		err error
	}
	jobs := make(chan int64)
	results := make(chan checkResult, len(ids))
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for id := range jobs {
				ctx, cancel := context.WithTimeout(parent, 45*time.Second)
				_, checkErr := a.refreshAccountQuota(ctx, id)
				cancel()
				results <- checkResult{id: id, err: checkErr}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-parent.Done():
				return
			}
		}
	}()
	wait.Wait()
	close(results)

	for item := range results {
		result.Checked++
		if item.err == nil {
			result.Healthy++
			continue
		}
		result.Failed++
		if len(result.Errors) < 20 {
			result.Errors = append(result.Errors, "#"+strconv.FormatInt(item.id, 10)+": "+item.err.Error())
		}
	}
	return result, nil
}

func (a *app) accountHealthIDs(requestedIDs []int64, manual bool) ([]int64, int, error) {
	requested := make(map[int64]struct{}, len(requestedIDs))
	for _, id := range requestedIDs {
		if id > 0 {
			requested[id] = struct{}{}
		}
	}
	query := `SELECT a.id, a.auth_type, a.auth_status, a.credentials_json, a.proxy_id
		FROM accounts a
		WHERE a.deleted_at IS NULL AND a.status != 'disabled' AND ` + legacyExecutionPredicate("a")
	args := make([]any, 0, len(requested))
	if len(requested) > 0 {
		placeholders := make([]string, 0, len(requested))
		for id := range requested {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		query += " AND a.id IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY a.id"
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	skipped := 0
	for rows.Next() {
		var id int64
		var authType, authStatus, credentialsJSON string
		var proxyID sql.NullInt64
		if err := rows.Scan(&id, &authType, &authStatus, &credentialsJSON, &proxyID); err != nil {
			return nil, skipped, err
		}
		eligible := authType == "oauth" && strings.TrimSpace(credentialsJSON) != "{}" && proxyID.Valid
		if !manual {
			eligible = eligible && authStatus != "reauth_required"
		}
		if !eligible {
			skipped++
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, skipped, err
	}
	if len(requested) > 0 {
		skipped += len(requested) - len(ids) - skipped
	}
	return ids, skipped, nil
}
