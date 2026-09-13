package app

import (
	"github.com/Wei-Shaw/sub2api/internal/portunex/apikeys"
	"github.com/Wei-Shaw/sub2api/internal/portunex/providers"
	"github.com/Wei-Shaw/sub2api/internal/portunex/users"
)

// Public synthetic fixtures. Values are not derived from production accounts,
// provider credentials, model traffic or database rows.
func demoUsers() users.Catalog {
	return users.New([]users.User{
		{ID: "u-admin", Name: "演示管理员", Email: "admin@example.invalid", Role: "admin", Status: "active", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "u-member", Name: "演示成员", Email: "member@example.invalid", Role: "user", Status: "active", CreatedAt: "2026-01-02T00:00:00Z"},
		{ID: "u-disabled", Name: "已停用演示成员", Email: "disabled@example.invalid", Role: "user", Status: "disabled", CreatedAt: "2026-01-03T00:00:00Z"},
	})
}

func demoKeys() apikeys.Catalog {
	lastUsed := "2026-01-04T00:00:00Z"
	return apikeys.New([]apikeys.Key{
		{ID: "k-admin", Name: "管理演示", KeyHint: "demo-••••-a001", Owner: "u-admin", Status: "active", LastUsedAt: &lastUsed},
		{ID: "k-member", Name: "成员演示", KeyHint: "demo-••••-m001", Owner: "u-member", Status: "active"},
		{ID: "k-disabled", Name: "停用演示", KeyHint: "demo-••••-d001", Owner: "u-disabled", Status: "disabled"},
	})
}

func demoProviders() providers.Catalog {
	return providers.New([]providers.Provider{
		{ID: "p-claude", Name: "Claude 本地演示", Type: "Claude", Status: "ready", ModelCount: 2, UpdatedAt: "2026-01-04T00:00:00Z"},
		{ID: "p-openai", Name: "OpenAI 本地演示", Type: "OpenAI", Status: "disabled", ModelCount: 3, UpdatedAt: "2026-01-04T00:00:00Z"},
		{ID: "p-gemini", Name: "Gemini 本地演示", Type: "Gemini", Status: "cooling", ModelCount: 1, UpdatedAt: "2026-01-04T00:00:00Z"},
	})
}
