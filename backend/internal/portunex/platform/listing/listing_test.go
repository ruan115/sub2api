package listing

import (
	"errors"
	"net/url"
	"testing"
)

func TestParseRejectsAmbiguousAndOutOfBoundsQueries(t *testing.T) {
	cases := []url.Values{
		{"unknown": {"value"}},
		{"query": {"first", "second"}},
		{"query": {"bad\x00value"}},
		{"page": {"0"}},
		{"page": {"1000001"}},
		{"page_size": {"101"}},
		{"page_size": {"not-a-number"}},
	}
	for _, values := range cases {
		if _, err := Parse(values); !errors.Is(err, ErrQuery) {
			t.Fatalf("Parse(%v) error = %v, want ErrQuery", values, err)
		}
	}
}

func TestParseAndPaginateBoundedSearch(t *testing.T) {
	query, err := Parse(url.Values{"query": {"  ALPHA "}, "page": {"2"}, "page_size": {"2"}})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if query != (Query{Search: "alpha", Page: 2, PageSize: 2}) {
		t.Fatalf("Parse() = %#v", query)
	}
	if !Match(query.Search, "first alpha") || Match(query.Search, "beta") {
		t.Fatalf("Match() did not apply expected case-insensitive search")
	}

	page := Paginate([]string{"one", "two", "three", "four", "five"}, query)
	if page.Total != 5 || page.Page != 2 || page.PageSize != 2 {
		t.Fatalf("page metadata = %#v", page)
	}
	if got, want := page.Items, []string{"three", "four"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("page items = %#v, want %#v", got, want)
	}

	invalid := Paginate([]string{"one"}, Query{Page: 0, PageSize: 20})
	if invalid.Total != 1 || len(invalid.Items) != 0 {
		t.Fatalf("invalid query pagination = %#v, want empty bounded page", invalid)
	}
}
