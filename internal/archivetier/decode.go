package archivetier

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// Every read that feeds a safety decision fails closed on absent or null
// collections and totals. Go decodes JSON null into an empty slice or a zero
// int, which would read "no queue" or "no recent viewing" as safe; pointers
// keep absent and null distinct from empty and zero.

// list decodes a JSON array that must be present and non-null.
func list[T any](ctx context.Context, c *Client, p string, q url.Values) ([]T, error) {
	var out *[]T
	if e := c.get(ctx, p, q, &out); e != nil {
		return nil, e
	}
	if out == nil {
		return nil, fmt.Errorf("%s %s: missing collection", c.Kind, p)
	}
	return *out, nil
}

// uniqueIDs rejects non-positive or repeated record identities.
func uniqueIDs[T any](items []T, id func(T) int) error {
	seen := map[int]bool{}
	for _, it := range items {
		n := id(it)
		if n <= 0 || seen[n] {
			return fmt.Errorf("invalid or repeated record identity")
		}
		seen[n] = true
	}
	return nil
}

type arrPage[T any] struct {
	Page     *int `json:"page"`
	PageSize *int `json:"pageSize"`
	Total    *int `json:"totalRecords"`
	Records  *[]T `json:"records"`
}

const pageSize = 100

// paged reads every page of an Arr paged collection. The page and size must
// echo the request, the total must be present, non-negative and stable, every
// record must have a unique positive identity, and the records must add up to
// exactly the total.
func paged[T any](ctx context.Context, c *Client, p string, q url.Values, id func(T) int) ([]T, error) {
	out := []T{}
	seen := map[int]bool{}
	total := -1
	for n := 1; n <= 100; n++ {
		v := url.Values{}
		for k, vs := range q {
			v[k] = append([]string(nil), vs...)
		}
		v.Set("page", strconv.Itoa(n))
		v.Set("pageSize", strconv.Itoa(pageSize))
		var pg arrPage[T]
		if e := c.get(ctx, p, v, &pg); e != nil {
			return nil, e
		}
		if pg.Total == nil || *pg.Total < 0 || pg.Records == nil || pg.Page == nil || *pg.Page != n || pg.PageSize == nil || *pg.PageSize != pageSize || len(*pg.Records) > pageSize {
			return nil, fmt.Errorf("%s %s: incomplete page", c.Kind, p)
		}
		if total >= 0 && *pg.Total != total {
			return nil, fmt.Errorf("%s %s: total changed during pagination", c.Kind, p)
		}
		total = *pg.Total
		for _, r := range *pg.Records {
			k := id(r)
			if k <= 0 || seen[k] {
				return nil, fmt.Errorf("%s %s: invalid or repeated record identity", c.Kind, p)
			}
			seen[k] = true
		}
		out = append(out, *pg.Records...)
		if len(out) == total {
			return out, nil
		}
		if len(out) > total || len(*pg.Records) < pageSize {
			return nil, fmt.Errorf("%s %s: records do not add up to the total", c.Kind, p)
		}
	}
	return nil, fmt.Errorf("%s %s: exceeds safety limit", c.Kind, p)
}
