// Package listing provides bounded query handling for the internal demo API.
package listing

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

var ErrQuery = errors.New("invalid list query")

type Query struct {
	Search   string
	Page     int
	PageSize int
}

type Page[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

func Parse(values url.Values) (Query, error) {
	q := Query{Page: 1, PageSize: 20}
	for key, items := range values {
		if len(items) != 1 {
			return Query{}, ErrQuery
		}
		switch key {
		case "query":
			if len(items[0]) > 128 || strings.IndexFunc(items[0], unicode.IsControl) >= 0 {
				return Query{}, ErrQuery
			}
			q.Search = strings.ToLower(strings.TrimSpace(items[0]))
		case "page", "page_size":
			value, err := strconv.Atoi(items[0])
			if err != nil || value < 1 {
				return Query{}, ErrQuery
			}
			if key == "page" {
				if value > 1000000 {
					return Query{}, ErrQuery
				}
				q.Page = value
			} else {
				if value > 100 {
					return Query{}, ErrQuery
				}
				q.PageSize = value
			}
		default:
			return Query{}, ErrQuery
		}
	}
	return q, nil
}

func Match(query string, fields ...string) bool {
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

func Paginate[T any](rows []T, query Query) Page[T] {
	result := Page[T]{Items: make([]T, 0), Total: len(rows), Page: query.Page, PageSize: query.PageSize}
	if query.Page < 1 || query.PageSize < 1 || query.PageSize > 100 || query.Page > 1000000 {
		return result
	}
	start := (query.Page - 1) * query.PageSize
	if start >= len(rows) {
		return result
	}
	end := min(start+query.PageSize, len(rows))
	result.Items = append(result.Items, rows[start:end]...)
	return result
}
