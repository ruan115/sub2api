package main

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"
)

type gatewayQueueState struct {
	serial chan struct{}
	mu     sync.Mutex
	last   time.Time
}

var gatewaySerialQueueTimeout = 2 * time.Minute

func (a *app) acquireUserMessageQueue(ctx context.Context, account gatewayAccount, body []byte, countTokens bool) (func(), error) {
	noop := func() {}
	if countTokens || (account.AuthType != "oauth" && account.AuthType != "setup_token") || account.UserMsgQueueMode == "" || account.UserMsgQueueMode == "off" || !isRealUserMessage(body) {
		return noop, nil
	}
	stateValue, _ := a.queueStates.LoadOrStore(account.ID, &gatewayQueueState{serial: make(chan struct{}, 1)})
	state := stateValue.(*gatewayQueueState)
	delay := a.userMessageDelay(account)
	if account.UserMsgQueueMode == "soft" {
		return noop, waitGatewayDelay(ctx, delay)
	}
	if account.UserMsgQueueMode != "serial" {
		return noop, nil
	}
	timer := time.NewTimer(gatewaySerialQueueTimeout)
	defer timer.Stop()
	select {
	case state.serial <- struct{}{}:
	case <-ctx.Done():
		return noop, ctx.Err()
	case <-timer.C:
		return noop, context.DeadlineExceeded
	}
	state.mu.Lock()
	last := state.last
	state.mu.Unlock()
	if !last.IsZero() {
		remaining := delay - time.Since(last)
		if remaining > 0 {
			if err := waitGatewayDelay(ctx, remaining); err != nil {
				<-state.serial
				return noop, err
			}
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			state.mu.Lock()
			state.last = time.Now()
			state.mu.Unlock()
			<-state.serial
		})
	}, nil
}

func waitGatewayDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *app) userMessageDelay(account gatewayAccount) time.Duration {
	if account.BaseRPM <= 0 {
		return 200 * time.Millisecond
	}
	var current int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, account.ID).Scan(&current)
	ratio := float64(current) / float64(account.BaseRPM)
	minDelay := 200 * time.Millisecond
	maxDelay := 2 * time.Second
	if ratio < 0.5 {
		return minDelay
	}
	if ratio >= 0.8 {
		return maxDelay
	}
	position := (ratio - 0.5) / 0.3
	return time.Duration(math.Round(float64(minDelay) + position*(float64(maxDelay)-float64(minDelay))))
}

func isRealUserMessage(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) == 0 {
		return false
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last["role"] != "user" {
		return false
	}
	content, ok := last["content"].([]any)
	if !ok {
		return true
	}
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool_result" || block["type"] == "tool_use_result" {
			return false
		}
	}
	return true
}
