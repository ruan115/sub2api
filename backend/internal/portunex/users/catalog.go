// Package users owns the demo user-list projection, not authentication storage.
package users

import "github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"

type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

type Catalog struct{ rows []User }

func New(rows []User) Catalog { return Catalog{rows: append([]User(nil), rows...)} }

func (c Catalog) List(query listing.Query) listing.Page[User] {
	rows := make([]User, 0)
	for _, row := range c.rows {
		if listing.Match(query.Search, row.ID, row.Name, row.Email, row.Role, row.Status) {
			rows = append(rows, row)
		}
	}
	return listing.Paginate(rows, query)
}
