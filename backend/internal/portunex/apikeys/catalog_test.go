package apikeys

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"
)

func TestCatalogFiltersOwnerBeforeSearchAndTotal(t *testing.T) {
	lastUsed := "2026-09-13T12:00:00Z"
	catalog := New([]Key{
		{ID: "a-1", Name: "shared own first", KeyHint: "pk-own-1", Owner: "owner-a", Status: "active", LastUsedAt: &lastUsed},
		{ID: "a-2", Name: "shared own second", KeyHint: "pk-own-2", Owner: "owner-a", Status: "active"},
		{ID: "b-1", Name: "shared other one", KeyHint: "pk-other-1", Owner: "owner-b", Status: "active"},
		{ID: "b-2", Name: "shared other two", KeyHint: "pk-other-2", Owner: "owner-b", Status: "active"},
	})

	page := catalog.List(listing.Query{Search: "shared", Page: 1, PageSize: 1}, "owner-a", false)
	if page.Total != 2 {
		t.Fatalf("member total = %d, want only the two owned keys", page.Total)
	}
	if len(page.Items) != 1 || page.Items[0].Owner != "owner-a" {
		t.Fatalf("member page = %#v, leaked another owner", page)
	}

	admin := catalog.List(listing.Query{Search: "shared", Page: 1, PageSize: 20}, "owner-a", true)
	if admin.Total != 4 {
		t.Fatalf("admin total = %d, want all keys", admin.Total)
	}
}

func TestCatalogDeepCopiesLastUsedAt(t *testing.T) {
	inputLastUsed := "2026-09-13T12:00:00Z"
	catalog := New([]Key{{ID: "a-1", Name: "owned", KeyHint: "pk-own", Owner: "owner-a", Status: "active", LastUsedAt: &inputLastUsed}})
	inputLastUsed = "input mutation"

	page := catalog.List(listing.Query{Page: 1, PageSize: 20}, "owner-a", false)
	if page.Items[0].LastUsedAt == nil || *page.Items[0].LastUsedAt != "2026-09-13T12:00:00Z" {
		t.Fatalf("input pointer mutation reached catalog: %#v", page.Items[0])
	}
	*page.Items[0].LastUsedAt = "output mutation"

	again := catalog.List(listing.Query{Page: 1, PageSize: 20}, "owner-a", false)
	if again.Items[0].LastUsedAt == nil || *again.Items[0].LastUsedAt != "2026-09-13T12:00:00Z" {
		t.Fatalf("output pointer mutation reached catalog: %#v", again.Items[0])
	}
}
