package orm

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// PageRequest is a page of a list as a client asks for it: Page from 1,
// Size, Sort (fields, a leading - for descending, comma-separated) and
// Search.
type PageRequest struct {
	Page, Size   int
	Sort, Search string
}

// PageFrom reads a PageRequest from query parameters page, size, sort
// and q.
func PageFrom(v url.Values) PageRequest {
	page, _ := strconv.Atoi(v.Get("page"))
	size, _ := strconv.Atoi(v.Get("size"))
	return PageRequest{Page: page, Size: size, Sort: v.Get("sort"), Search: v.Get("q")}
}

// PageResult is a page of rows and where it sits in the list.
type PageResult[T any] struct {
	Items []T
	Total int64
	Page  int
	Size  int
	Pages int
}

// PageOption shapes Paginate: the fields a client may sort and search by,
// the default order, the largest page.
type PageOption func(*pageConfig)

type pageConfig struct {
	sortable, searchable []string
	def                  []string
	maxSize, size        int
}

// Sortable is the fields a client may sort by; others are ignored. Paths
// through foreign keys (author__name) are fields too.
func Sortable(fields ...string) PageOption {
	return func(c *pageConfig) { c.sortable = append(c.sortable, fields...) }
}

// Searchable is the fields Search matches, case-insensitively, any of
// them.
func Searchable(fields ...string) PageOption {
	return func(c *pageConfig) { c.searchable = append(c.searchable, fields...) }
}

// DefaultSort is the order when the client asks for none (or none it may
// have).
func DefaultSort(fields ...string) PageOption { return func(c *pageConfig) { c.def = fields } }

// MaxSize caps the page size (100 by default); DefaultSize is the size
// asked for none (20).
func MaxSize(n int) PageOption     { return func(c *pageConfig) { c.maxSize = n } }
func DefaultSize(n int) PageOption { return func(c *pageConfig) { c.size = n } }

// maxSearch is the longest search Paginate runs, in bytes.
const maxSearch = 200

// Paginate is one page of qs as r asks for it, sorted and searched only by
// the fields opts allow — what a REST list or a view's DataTable shows.
//
//	page, err := orm.Paginate(ctx, Users.Filter(orm.Q{"active": true}), orm.PageFrom(c.Request.URL.Query()),
//		orm.Sortable("name", "created_at"), orm.Searchable("name", "email"), orm.DefaultSort("-created_at"))
func Paginate[T any](ctx context.Context, qs QuerySet[T], r PageRequest, opts ...PageOption) (PageResult[T], error) {
	c := pageConfig{maxSize: 100, size: 20}
	for _, o := range opts {
		o(&c)
	}
	size := r.Size
	if size <= 0 {
		size = c.size
	}
	size = max(min(size, c.maxSize), 1)
	page := max(r.Page, 1)
	search := strings.TrimSpace(r.Search)
	if len(search) > maxSearch {
		search = strings.ToValidUTF8(search[:maxSearch], "")
	}
	if s := search; s != "" && len(c.searchable) > 0 {
		var any []Cond
		for _, f := range c.searchable {
			any = append(any, Q{f + "__icontains": s})
		}
		qs = qs.Filter(Or(any...))
	}
	var order []string
	sorted := map[string]bool{}
	for part := range strings.SplitSeq(r.Sort, ",") {
		part = strings.TrimSpace(part)
		f := strings.TrimPrefix(part, "-")
		// Each field once: a query string repeating one can't make an
		// ORDER BY of thousands of terms.
		if slices.Contains(c.sortable, f) && f != "" && !sorted[f] {
			sorted[f] = true
			order = append(order, part)
		}
	}
	if len(order) == 0 {
		order = c.def
	}
	if len(order) > 0 {
		qs = qs.OrderBy(order...)
	}
	total, err := qs.Count(ctx)
	if err != nil {
		return PageResult[T]{}, err
	}
	pages := int((total + int64(size) - 1) / int64(size))
	// A page past the end is empty, without asking: a huge page number
	// can't overflow the offset or make the database skip that far.
	if int64(page-1) >= (total+int64(size)-1)/int64(size) {
		return PageResult[T]{Items: []T{}, Total: total, Page: page, Size: size, Pages: pages}, nil
	}
	items, err := qs.Offset((page - 1) * size).Limit(size).All(ctx)
	if err != nil {
		return PageResult[T]{}, err
	}
	return PageResult[T]{Items: items, Total: total, Page: page, Size: size, Pages: pages}, nil
}
