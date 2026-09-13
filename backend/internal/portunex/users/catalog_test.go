package users

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"
)

func TestCatalogCopiesInputAndResults(t *testing.T) {
	input := []User{{ID: "u-1", Name: "Original", Email: "member@example.invalid", Role: "user", Status: "active"}}
	catalog := New(input)
	input[0].Name = "Mutated input"

	page := catalog.List(listing.Query{Page: 1, PageSize: 20})
	if page.Total != 1 || page.Items[0].Name != "Original" {
		t.Fatalf("catalog after input mutation = %#v", page)
	}
	page.Items[0].Name = "Mutated output"

	again := catalog.List(listing.Query{Page: 1, PageSize: 20})
	if again.Items[0].Name != "Original" {
		t.Fatalf("catalog result mutation changed stored row: %#v", again)
	}
}
