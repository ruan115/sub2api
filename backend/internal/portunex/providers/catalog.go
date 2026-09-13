// Package providers exposes synthetic metadata only; credentials have no field.
package providers

import "github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"

type Provider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	ModelCount int    `json:"modelCount"`
	UpdatedAt  string `json:"updatedAt"`
}

type Catalog struct{ rows []Provider }

func New(rows []Provider) Catalog { return Catalog{rows: append([]Provider(nil), rows...)} }

func (c Catalog) List(query listing.Query) listing.Page[Provider] {
	rows := make([]Provider, 0)
	for _, row := range c.rows {
		if listing.Match(query.Search, row.ID, row.Name, row.Type, row.Status) {
			rows = append(rows, row)
		}
	}
	return listing.Paginate(rows, query)
}
