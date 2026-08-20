package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type gatewayKey struct {
	ID        int64
	UserID    int64
	GroupID   string
	Quota     float64
	QuotaUsed float64
	UserRPM   int
	Allowed   string
	UserRole  string
	ExpiresAt sql.NullString
}

type gatewayAccount struct {
	ID               int64
	Name             string
	AuthType         string
	CredentialsJSON  string
	ExtraJSON        string
	Concurrency      int
	BaseRPM          int
	RPMStrategy      string
	StickyBuffer     int
	UserMsgQueueMode string
	ProxyID          sql.NullInt64
}

type messageEnvelope struct {
	Model    string         `json:"model"`
	Stream   bool           `json:"stream"`
	Metadata map[string]any `json:"metadata"`
}

type tokenUsage struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

func (a *app) handleMessages(w http.ResponseWriter, r *http.Request) {
	a.handleClaudeGateway(w, r, false)
}

func (a *app) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	a.handleClaudeGateway(w, r, true)
}

type gatewayUpstreamFailure struct {
	status  int
	header  http.Header
	body    []byte
	account gatewayAccount
}

func (a *app) handleClaudeGateway(w http.ResponseWriter, r *http.Request, countTokens bool) {
	secret := bearerOrAPIKey(r)
	if secret == "" {
		writeError(w, http.StatusUnauthorized, "API key is required")
		return
	}
	key, err := a.authenticateGatewayKey(secret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or unavailable API key")
		return
	}
	if key.Quota > 0 && key.QuotaUsed >= key.Quota {
		writeError(w, http.StatusForbidden, "API key quota exhausted")
		return
	}
	if ok := groupAllowedJSON(key.UserRole, key.Allowed, key.GroupID); !ok {
		writeError(w, http.StatusForbidden, "API key group is no longer allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large or invalid")
		return
	}
	var envelope messageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || strings.TrimSpace(envelope.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return
	}
	if err := a.checkAndIncrementUserRPM(key); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	session := messageSession(r, envelope.Metadata)
	excluded := map[int64]bool{}
	var lastFailure *gatewayUpstreamFailure
	var lastDispatchError error
	for attempt := 0; attempt < gatewayMaxAttempts(); attempt++ {
		account, acquireErr := a.acquireGatewayAccount(key, session, envelope.Model, excluded)
		if acquireErr != nil {
			lastDispatchError = acquireErr
			break
		}
		excluded[account.ID] = true
		account, err = a.ensureGatewayAccountToken(r.Context(), account)
		if err != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = err
			continue
		}
		prepared, prepareErr := prepareClaudeRequest(r, body, account, envelope.Model, countTokens)
		if prepareErr != nil {
			a.releaseGatewayAccount(account.ID)
			writeError(w, http.StatusBadRequest, prepareErr.Error())
			return
		}
		path := "/v1/messages"
		if countTokens {
			path = "/v1/messages/count_tokens"
		}
		upstreamURL, urlErr := upstreamClaudeURL(account.ExtraJSON, path)
		if urlErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = urlErr
			continue
		}
		upstreamRequest, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(prepared.Body))
		if requestErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = requestErr
			continue
		}
		if headerErr := buildClaudeHeaders(upstreamRequest.Header, r.Header, prepared, account.CredentialsJSON); headerErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = headerErr
			continue
		}
		if !account.ProxyID.Valid {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = errors.New("CCMAX account must bind an active proxy")
			continue
		}
		proxyURL, err := a.proxyURL(account.ProxyID.Int64)
		if err != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = errors.New("CCMAX account proxy is unavailable")
			continue
		}
		client, clientErr := clientForProxy(proxyURL)
		if clientErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = clientErr
			continue
		}
		queueRelease, queueErr := a.acquireUserMessageQueue(r.Context(), account, body, countTokens)
		if queueErr != nil && r.Context().Err() != nil {
			a.releaseGatewayAccount(account.ID)
			return
		}
		started := time.Now()
		response, requestErr := client.Do(upstreamRequest)
		queueRelease()
		if requestErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = requestErr
			continue
		}
		a.captureAccountUpstreamState(account.ID, response)
		if retryableGatewayStatus(response.StatusCode) {
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			a.releaseGatewayAccount(account.ID)
			lastFailure = &gatewayUpstreamFailure{status: response.StatusCode, header: response.Header.Clone(), body: failureBody, account: account}
			continue
		}
		usage, forwardErr := forwardGatewayResponse(w, response, prepared.Stream && !countTokens, account, key.GroupID)
		response.Body.Close()
		a.releaseGatewayAccount(account.ID)
		if forwardErr != nil {
			return
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if countTokens {
				_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
				return
			}
			a.recordGatewayUsage(key, account, envelope.Model, prepared.Stream, response.Header.Get("request-id"), usage, started)
		}
		return
	}
	if lastFailure != nil {
		copyGatewayResponseHeaders(w.Header(), lastFailure.header)
		w.Header().Set("X-CCMAX-Account", lastFailure.account.Name)
		w.Header().Set("X-CCMAX-Group", key.GroupID)
		w.WriteHeader(lastFailure.status)
		_, _ = w.Write(lastFailure.body)
		return
	}
	status := http.StatusServiceUnavailable
	message := "no account is available for this request"
	if lastDispatchError != nil {
		message = lastDispatchError.Error()
		if strings.Contains(message, "RPM") || strings.Contains(message, "capacity") {
			status = http.StatusTooManyRequests
		}
	}
	writeError(w, status, message)
}

func gatewayMaxAttempts() int {
	return 4
}

func retryableGatewayStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func forwardGatewayResponse(w http.ResponseWriter, response *http.Response, stream bool, account gatewayAccount, groupID string) (tokenUsage, error) {
	copyGatewayResponseHeaders(w.Header(), response.Header)
	w.Header().Set("X-CCMAX-Account", account.Name)
	w.Header().Set("X-CCMAX-Group", groupID)
	if !stream || response.StatusCode < 200 || response.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		if err != nil {
			writeError(w, http.StatusBadGateway, "failed to read upstream response")
			return tokenUsage{}, err
		}
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(body)
		return parseAnthropicUsage(body, false), err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(response.Body, 64<<10)
	usage := tokenUsage{}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			usage = mergeTokenUsage(usage, parseAnthropicUsage(line, true))
			if _, writeErr := w.Write(line); writeErr != nil {
				return usage, writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return usage, nil
			}
			return usage, err
		}
	}
}

func mergeTokenUsage(current, next tokenUsage) tokenUsage {
	current.Input = max(current.Input, next.Input)
	current.Output = max(current.Output, next.Output)
	current.CacheCreation = max(current.CacheCreation, next.CacheCreation)
	current.CacheRead = max(current.CacheRead, next.CacheRead)
	return current
}

func copyGatewayResponseHeaders(target, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func (a *app) recordGatewayUsage(key gatewayKey, account gatewayAccount, model string, stream bool, requestID string, usage tokenUsage, started time.Time) {
	item, _, usageErr := a.recordUsage(usageInput{
		RequestID: requestID, PurposeKey: "default", GroupID: key.GroupID, AccountID: account.ID, Model: model,
		InputTokens: usage.Input, OutputTokens: usage.Output, CacheCreationTokens: usage.CacheCreation,
		CacheReadTokens: usage.CacheRead, Stream: stream, DurationMS: int(time.Since(started).Milliseconds()),
	})
	if usageErr == nil {
		_, _ = a.db.Exec(`UPDATE api_keys SET quota_used = quota_used + ?, last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, item.BilledCost, key.ID)
		return
	}
	_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
}

func bearerOrAPIKey(r *http.Request) string {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func (a *app) authenticateGatewayKey(secret string) (gatewayKey, error) {
	var key gatewayKey
	err := a.db.QueryRow(`SELECT k.id, k.user_id, k.group_id, k.quota, k.quota_used, u.rpm_limit, u.allowed_group_ids_json, u.role, k.expires_at
		FROM api_keys k JOIN users u ON u.id = k.user_id JOIN groups g ON g.id = k.group_id
		WHERE k.key_hash = ? AND k.status = 'active' AND k.deleted_at IS NULL AND u.status = 'active' AND u.deleted_at IS NULL AND g.status = 'active'
		AND (k.expires_at IS NULL OR k.expires_at > `+nowSQL+`)`, hashToken(secret)).Scan(&key.ID, &key.UserID, &key.GroupID, &key.Quota, &key.QuotaUsed, &key.UserRPM, &key.Allowed, &key.UserRole, &key.ExpiresAt)
	return key, err
}

type gatewayModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
}

func newGatewayModel(id, createdAt string) gatewayModel {
	created := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		created = parsed
	}
	return gatewayModel{
		ID:          id,
		Type:        "model",
		DisplayName: id,
		CreatedAt:   createdAt,
		Object:      "model",
		Created:     created.Unix(),
		OwnedBy:     "anthropic",
	}
}

func (a *app) handleModels(w http.ResponseWriter, r *http.Request) {
	secret := bearerOrAPIKey(r)
	if secret == "" {
		writeError(w, http.StatusUnauthorized, "API key is required")
		return
	}
	key, err := a.authenticateGatewayKey(secret)
	if err != nil || !groupAllowedJSON(key.UserRole, key.Allowed, key.GroupID) {
		writeError(w, http.StatusUnauthorized, "invalid or unavailable API key")
		return
	}
	models, err := a.gatewayModels(key.GroupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list models")
		return
	}
	requested := strings.TrimSpace(r.PathValue("id"))
	if requested != "" {
		for _, model := range models {
			if model.ID == requested {
				writeJSON(w, http.StatusOK, model)
				return
			}
		}
		writeError(w, http.StatusNotFound, "model not found")
		return
	}
	result := map[string]any{"object": "list", "data": models, "has_more": false, "first_id": nil, "last_id": nil}
	if len(models) > 0 {
		result["first_id"] = models[0].ID
		result["last_id"] = models[len(models)-1].ID
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) gatewayModels(groupID string) ([]gatewayModel, error) {
	models := map[string]gatewayModel{}
	priceRows, err := a.db.Query(`SELECT model, created_at FROM model_prices WHERE model != '*' ORDER BY model`)
	if err != nil {
		return nil, err
	}
	for priceRows.Next() {
		var id, createdAt string
		if err := priceRows.Scan(&id, &createdAt); err != nil {
			priceRows.Close()
			return nil, err
		}
		models[id] = newGatewayModel(id, createdAt)
	}
	if err := priceRows.Err(); err != nil {
		priceRows.Close()
		return nil, err
	}
	priceRows.Close()
	accountRows, err := a.db.Query(`SELECT a.extra_json FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND a.status = 'active' AND a.schedulable = 1 AND a.auth_status != 'reauth_required'
		AND EXISTS (SELECT 1 FROM proxies p WHERE p.id = a.proxy_id AND p.status = 'active' AND p.deleted_at IS NULL)`, groupID)
	if err != nil {
		return nil, err
	}
	restricted := map[string]bool{}
	patterns := []string{}
	unrestricted := false
	for accountRows.Next() {
		var raw string
		if err := accountRows.Scan(&raw); err != nil {
			accountRows.Close()
			return nil, err
		}
		extra := decodeObject(raw)
		configured := configuredModelNames(extra)
		if len(configured) == 0 || configured["*"] {
			unrestricted = true
		}
		for model := range configured {
			if strings.HasSuffix(model, "*") && model != "*" {
				patterns = append(patterns, model)
			} else if model != "*" {
				restricted[model] = true
			}
		}
		if mapping, ok := extra["model_mapping"].(map[string]any); ok {
			for model := range mapping {
				restricted[model] = true
			}
		}
	}
	if err := accountRows.Err(); err != nil {
		accountRows.Close()
		return nil, err
	}
	accountRows.Close()
	if !unrestricted {
		for model := range models {
			allowed := restricted[model]
			for _, pattern := range patterns {
				allowed = allowed || modelPatternMatches(pattern, model)
			}
			if !allowed {
				delete(models, model)
			}
		}
	}
	for model := range restricted {
		if _, ok := models[model]; !ok {
			models[model] = newGatewayModel(model, time.Now().UTC().Format(time.RFC3339))
		}
	}
	result := make([]gatewayModel, 0, len(models))
	for _, model := range models {
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func configuredModelNames(extra map[string]any) map[string]bool {
	result := map[string]bool{}
	for _, key := range []string{"supported_models", "available_models"} {
		switch values := extra[key].(type) {
		case []any:
			for _, value := range values {
				if model, ok := value.(string); ok && strings.TrimSpace(model) != "" {
					result[strings.TrimSpace(model)] = true
				}
			}
		case string:
			for _, model := range strings.Split(values, ",") {
				if strings.TrimSpace(model) != "" {
					result[strings.TrimSpace(model)] = true
				}
			}
		}
	}
	return result
}

func groupAllowedJSON(role, raw, groupID string) bool {
	if role == "admin" || role == "readonly_admin" {
		return true
	}
	groups := []string{}
	_ = json.Unmarshal([]byte(raw), &groups)
	for _, group := range groups {
		if group == groupID {
			return true
		}
	}
	return false
}

func (a *app) checkAndIncrementUserRPM(key gatewayKey) error {
	if key.UserRPM <= 0 {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`DELETE FROM user_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`)
	var current int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_rpm_events WHERE user_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, key.UserID).Scan(&current); err != nil {
		return err
	}
	if current >= key.UserRPM {
		return errors.New("user RPM limit reached")
	}
	if _, err := tx.Exec(`INSERT INTO user_rpm_events (user_id) VALUES (?)`, key.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

func messageSession(r *http.Request, metadata map[string]any) string {
	for _, header := range []string{"x-session-id", "session-id"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return hashToken(value)
		}
	}
	if metadata != nil {
		if value, ok := metadata["user_id"].(string); ok && value != "" {
			return hashToken(value)
		}
	}
	return ""
}

func (a *app) acquireGatewayAccount(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool) (gatewayAccount, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return gatewayAccount{}, err
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`DELETE FROM account_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`)
	_, _ = tx.Exec(`DELETE FROM dispatch_sessions WHERE expires_at <= ` + nowSQL)
	var stickyID int64
	if sessionHash != "" {
		_ = tx.QueryRow(`SELECT account_id FROM dispatch_sessions WHERE session_hash = ? AND api_key_id = ? AND expires_at > `+nowSQL, sessionHash, key.ID).Scan(&stickyID)
	}
	rows, err := tx.Query(`SELECT a.id, a.name, a.auth_type, a.credentials_json, a.extra_json, a.concurrency, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode, a.proxy_id,
		COALESCE((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0),
		COALESCE((SELECT requests FROM account_inflight f WHERE f.account_id = a.id), 0)
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND `+accountStatePredicate("a", "normal")+`
		ORDER BY CASE WHEN a.id = ? THEN 0 ELSE 1 END, ag.priority, a.priority, COALESCE(a.last_used_at, ''), a.id`, key.GroupID, stickyID)
	if err != nil {
		return gatewayAccount{}, err
	}
	type candidate struct {
		account  gatewayAccount
		rpm      int
		inflight int
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.account.ID, &item.account.Name, &item.account.AuthType, &item.account.CredentialsJSON, &item.account.ExtraJSON, &item.account.Concurrency, &item.account.BaseRPM, &item.account.RPMStrategy, &item.account.StickyBuffer, &item.account.UserMsgQueueMode, &item.account.ProxyID, &item.rpm, &item.inflight); err != nil {
			rows.Close()
			return gatewayAccount{}, err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	var selected gatewayAccount
	for _, candidate := range candidates {
		if excluded[candidate.account.ID] || !accountSupportsModel(candidate.account, requestedModel) {
			continue
		}
		if candidate.inflight >= candidate.account.Concurrency {
			continue
		}
		sticky := stickyID == candidate.account.ID
		if !rpmSchedulable(candidate.account, candidate.rpm, sticky) {
			continue
		}
		selected = candidate.account
		break
	}
	if selected.ID == 0 {
		return gatewayAccount{}, errors.New("no account capacity or model support available for group " + strings.ToUpper(key.GroupID) + " (model, concurrency, or RPM limit)")
	}
	if _, err := tx.Exec(`INSERT INTO account_rpm_events (account_id) VALUES (?)`, selected.ID); err != nil {
		return gatewayAccount{}, err
	}
	if _, err := tx.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 1) ON CONFLICT(account_id) DO UPDATE SET requests = requests + 1`, selected.ID); err != nil {
		return gatewayAccount{}, err
	}
	if sessionHash != "" {
		expires := time.Now().UTC().Add(15 * time.Minute).Format(time.RFC3339Nano)
		_, err = tx.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at) VALUES (?, ?, ?, ?) ON CONFLICT(session_hash, api_key_id) DO UPDATE SET account_id = excluded.account_id, expires_at = excluded.expires_at`, sessionHash, key.ID, selected.ID, expires)
		if err != nil {
			return gatewayAccount{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE accounts SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, selected.ID); err != nil {
		return gatewayAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return gatewayAccount{}, err
	}
	return selected, nil
}

func rpmSchedulable(account gatewayAccount, current int, sticky bool) bool {
	if account.BaseRPM <= 0 || current < account.BaseRPM {
		return true
	}
	if account.RPMStrategy == "sticky_exempt" {
		return sticky
	}
	buffer := account.StickyBuffer
	if buffer <= 0 {
		buffer = account.Concurrency
		floor := account.BaseRPM / 5
		if floor < 1 {
			floor = 1
		}
		if buffer < floor {
			buffer = floor
		}
	}
	return current < account.BaseRPM+buffer && sticky
}

func (a *app) releaseGatewayAccount(accountID int64) {
	_, _ = a.db.Exec(`UPDATE account_inflight SET requests = CASE WHEN requests > 0 THEN requests - 1 ELSE 0 END WHERE account_id = ?`, accountID)
}

func upstreamClaudeURL(extraJSON, endpointPath string) (string, error) {
	extra := decodeObject(extraJSON)
	value, _ := extra["custom_forward_url"].(string)
	if strings.TrimSpace(value) == "" {
		value, _ = extra["base_url"].(string)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "https://api.anthropic.com" + endpointPath + "?beta=true", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("account custom forward URL is invalid")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("custom forward URL must use HTTPS")
		}
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/v1/messages/count_tokens", "/v1/messages"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	parsed.Path = strings.TrimRight(path, "/") + endpointPath
	query := parsed.Query()
	query.Set("beta", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func parseAnthropicUsage(body []byte, stream bool) tokenUsage {
	usage := tokenUsage{}
	var apply func(map[string]any)
	apply = func(value map[string]any) {
		if nested, ok := value["usage"].(map[string]any); ok {
			usage.Input = max(usage.Input, intFromJSON(nested["input_tokens"]))
			usage.Output = max(usage.Output, intFromJSON(nested["output_tokens"]))
			usage.CacheCreation = max(usage.CacheCreation, intFromJSON(nested["cache_creation_input_tokens"]))
			usage.CacheRead = max(usage.CacheRead, intFromJSON(nested["cache_read_input_tokens"]))
		}
		if message, ok := value["message"].(map[string]any); ok {
			apply(message)
		}
	}
	if !stream {
		value := map[string]any{}
		_ = json.Unmarshal(body, &value)
		apply(value)
		return usage
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := map[string]any{}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &value) == nil {
			apply(value)
		}
	}
	return usage
}

func intFromJSON(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		result, _ := number.Int64()
		return result
	default:
		return 0
	}
}
