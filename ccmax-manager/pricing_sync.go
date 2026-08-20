package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type priceSyncController struct {
	mu sync.Mutex
}

type pricingSyncState struct {
	RemoteURL     string `json:"remote_url"`
	HashURL       string `json:"hash_url"`
	RemoteHash    string `json:"remote_hash"`
	Status        string `json:"status"`
	ModelCount    int    `json:"model_count"`
	LastSyncedAt  string `json:"last_synced_at"`
	LastCheckedAt string `json:"last_checked_at"`
	LastError     string `json:"last_error"`
}

type remotePrice struct {
	InputCostPerToken         float64 `json:"input_cost_per_token"`
	OutputCostPerToken        float64 `json:"output_cost_per_token"`
	CacheCreationCostPerToken float64 `json:"cache_creation_input_token_cost"`
	CacheReadCostPerToken     float64 `json:"cache_read_input_token_cost"`
	LiteLLMProvider           string  `json:"litellm_provider"`
	Mode                      string  `json:"mode"`
}

func newPriceSyncController() *priceSyncController {
	return &priceSyncController{}
}

func (a *app) startPriceSyncScheduler() func() {
	if disabled := strings.TrimSpace(os.Getenv("CCMAX_PRICING_AUTO_SYNC")); disabled == "0" || strings.EqualFold(disabled, "false") {
		return func() {}
	}
	minutes := 10
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CCMAX_PRICING_SYNC_MINUTES"))); err == nil && value > 0 {
		minutes = value
	}
	stop := make(chan struct{})
	go func() {
		if _, err := a.syncModelPrices(context.Background(), false); err != nil {
			log.Printf("pricing initial sync: %v", err)
		}
		ticker := time.NewTicker(time.Duration(minutes) * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := a.syncModelPrices(context.Background(), false); err != nil {
					log.Printf("pricing scheduled sync: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

func (a *app) handlePriceSyncStatus(w http.ResponseWriter, _ *http.Request) {
	state, err := a.getPriceSyncState()
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *app) handlePriceSync(w http.ResponseWriter, r *http.Request) {
	state, err := a.syncModelPrices(r.Context(), true)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *app) getPriceSyncState() (pricingSyncState, error) {
	var state pricingSyncState
	var synced, checked *string
	err := a.db.QueryRow(`SELECT remote_url, hash_url, remote_hash, status, model_count, last_synced_at, last_checked_at, last_error FROM pricing_sync_state WHERE id = 1`).Scan(&state.RemoteURL, &state.HashURL, &state.RemoteHash, &state.Status, &state.ModelCount, &synced, &checked, &state.LastError)
	if synced != nil {
		state.LastSyncedAt = *synced
	}
	if checked != nil {
		state.LastCheckedAt = *checked
	}
	return state, err
}

func (a *app) syncModelPrices(parent context.Context, force bool) (pricingSyncState, error) {
	a.priceSync.mu.Lock()
	defer a.priceSync.mu.Unlock()
	state, err := a.getPriceSyncState()
	if err != nil {
		return state, err
	}
	if value := strings.TrimSpace(os.Getenv("CCMAX_PRICING_REMOTE_URL")); value != "" {
		state.RemoteURL = value
	}
	if value := strings.TrimSpace(os.Getenv("CCMAX_PRICING_HASH_URL")); value != "" {
		state.HashURL = value
	}
	if !validPricingSyncURL(state.RemoteURL) || (state.HashURL != "" && !validPricingSyncURL(state.HashURL)) {
		return state, errors.New("pricing sync URLs must use HTTPS")
	}
	_, _ = a.db.Exec(`UPDATE pricing_sync_state SET status = 'syncing', last_checked_at = ` + nowSQL + `, last_error = '' WHERE id = 1`)
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	remoteHash := ""
	if state.HashURL != "" {
		if body, fetchErr := fetchPricingResource(ctx, state.HashURL, 1<<20); fetchErr == nil {
			if fields := strings.Fields(string(body)); len(fields) > 0 {
				remoteHash = fields[0]
			}
		}
	}
	if !force && remoteHash != "" && strings.EqualFold(remoteHash, state.RemoteHash) {
		_, _ = a.db.Exec(`UPDATE pricing_sync_state SET status = 'current', remote_url = ?, hash_url = ?, last_checked_at = `+nowSQL+`, last_error = '' WHERE id = 1`, state.RemoteURL, state.HashURL)
		return a.getPriceSyncState()
	}
	body, err := fetchPricingResource(ctx, state.RemoteURL, 32<<20)
	if err != nil {
		a.setPriceSyncError(err)
		return state, err
	}
	dataHash := sha256.Sum256(body)
	if remoteHash == "" {
		remoteHash = hex.EncodeToString(dataHash[:])
	}
	var source map[string]remotePrice
	if err := json.Unmarshal(body, &source); err != nil {
		err = fmt.Errorf("decode pricing data: %w", err)
		a.setPriceSyncError(err)
		return state, err
	}
	type item struct {
		model                                 string
		input, output, cacheCreate, cacheRead float64
	}
	items := make([]item, 0, len(source))
	for model, price := range source {
		lowerModel := strings.ToLower(model)
		provider := strings.ToLower(price.LiteLLMProvider)
		if provider != "anthropic" && !strings.Contains(lowerModel, "claude") {
			continue
		}
		if price.InputCostPerToken <= 0 && price.OutputCostPerToken <= 0 {
			continue
		}
		items = append(items, item{
			model: model, input: price.InputCostPerToken * 1_000_000, output: price.OutputCostPerToken * 1_000_000,
			cacheCreate: price.CacheCreationCostPerToken * 1_000_000, cacheRead: price.CacheReadCostPerToken * 1_000_000,
		})
	}
	if len(items) == 0 {
		err = errors.New("pricing source contained no Anthropic models")
		a.setPriceSyncError(err)
		return state, err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM model_prices WHERE source = 'remote'`); err != nil {
		return state, err
	}
	for _, entry := range items {
		_, err = tx.Exec(`INSERT INTO model_prices (model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million, source, source_hash) VALUES (?, ?, ?, ?, ?, 'remote', ?) ON CONFLICT(model) DO UPDATE SET input_per_million = excluded.input_per_million, output_per_million = excluded.output_per_million, cache_creation_per_million = excluded.cache_creation_per_million, cache_read_per_million = excluded.cache_read_per_million, source_hash = excluded.source_hash, updated_at = `+nowSQL+` WHERE model_prices.source = 'remote'`, entry.model, entry.input, entry.output, entry.cacheCreate, entry.cacheRead, remoteHash)
		if err != nil {
			return state, err
		}
	}
	if _, err := tx.Exec(`UPDATE pricing_sync_state SET remote_url = ?, hash_url = ?, remote_hash = ?, status = 'current', model_count = ?, last_synced_at = `+nowSQL+`, last_checked_at = `+nowSQL+`, last_error = '' WHERE id = 1`, state.RemoteURL, state.HashURL, remoteHash, len(items)); err != nil {
		return state, err
	}
	if err := tx.Commit(); err != nil {
		return state, err
	}
	return a.getPriceSyncState()
}

func validPricingSyncURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	host := parsed.Hostname()
	return parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func (a *app) setPriceSyncError(err error) {
	message := err.Error()
	if len(message) > 800 {
		message = message[:800]
	}
	_, _ = a.db.Exec(`UPDATE pricing_sync_state SET status = 'error', last_checked_at = `+nowSQL+`, last_error = ? WHERE id = 1`, message)
}

func fetchPricingResource(ctx context.Context, resourceURL string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json,text/plain")
	request.Header.Set("User-Agent", "ccmax-manager-pricing-sync/1.0")
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch pricing resource: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing resource returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("pricing resource is too large")
	}
	return body, nil
}
