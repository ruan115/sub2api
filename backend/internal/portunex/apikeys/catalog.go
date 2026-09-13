// Package apikeys owns a redacted metadata projection, never real key material.
package apikeys

import "github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"

type Key struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	KeyHint    string  `json:"keyHint"`
	Owner      string  `json:"owner"`
	Status     string  `json:"status"`
	LastUsedAt *string `json:"lastUsedAt"`
}

type Catalog struct{ rows []Key }

func New(rows []Key) Catalog {
	c := Catalog{rows: make([]Key, len(rows))}
	for i, row := range rows {
		c.rows[i] = clone(row)
	}
	return c
}

// Owner filtering precedes both search and pagination; a member cannot infer
// other owners' counts from the total field or query matches.
func (c Catalog) List(query listing.Query, ownerID string, admin bool) listing.Page[Key] {
	rows := make([]Key, 0)
	for _, row := range c.rows {
		if !admin && row.Owner != ownerID {
			continue
		}
		if listing.Match(query.Search, row.ID, row.Name, row.KeyHint, row.Owner, row.Status) {
			rows = append(rows, clone(row))
		}
	}
	return listing.Paginate(rows, query)
}

func clone(row Key) Key {
	if row.LastUsedAt != nil {
		value := *row.LastUsedAt
		row.LastUsedAt = &value
	}
	return row
}
