package web

import (
	"net/url"
	"slices"
	"strconv"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// pageSizes are the page sizes the stack page offers, in state changes per page.
var pageSizes = []int{10, 25, 50, 100}

// stackQuery is the stack page's position and view: at most one cursor, the page size and
// whether previews are expanded.
type stackQuery struct {
	Before *store.Cursor // older than this; nil with After nil is the latest page
	After  *store.Cursor // newer than this; resolved to Before before the page is read
	Limit  int
	Expand bool
}

// parseStackQuery reads before=, after=, limit= and previews=. It rejects a malformed
// cursor, both cursors at once, and a size that is not offered.
func parseStackQuery(v url.Values) (stackQuery, bool) {
	q := stackQuery{Limit: runsPageSize, Expand: v.Get("previews") == "all"}
	for _, c := range []struct {
		key string
		dst **store.Cursor
	}{{"before", &q.Before}, {"after", &q.After}} {
		if raw := v.Get(c.key); raw != "" {
			cur, ok := parseCursor(raw)
			if !ok {
				return stackQuery{}, false
			}
			*c.dst = cur
		}
	}
	if q.Before != nil && q.After != nil {
		return stackQuery{}, false
	}
	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !slices.Contains(pageSizes, n) {
			return stackQuery{}, false
		}
		q.Limit = n
	}
	return q, true
}

// values encodes q, leaving out what is default so plain links stay plain.
func (q stackQuery) values() url.Values {
	v := url.Values{}
	if q.Before != nil {
		v.Set("before", formatCursor(q.Before))
	}
	if q.After != nil {
		v.Set("after", formatCursor(q.After))
	}
	if q.Limit != runsPageSize {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Expand {
		v.Set("previews", "all")
	}
	return v
}

func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// sizeLink is one entry of the page size choice; the current size is not a link.
type sizeLink struct {
	N       int
	URL     string
	Current bool
}

// pager holds the stack page's links. An empty URL means the link does not apply.
type pager struct {
	Latest, Newer, Older string
	Toggle               string // the same page with previews folded or expanded
	Live                 string // the rail fragment, on the latest page only
	Sizes                []sizeLink
}

// buildPager links the page q showed, given its state changes (newest first) and the
// cursor to the next older page.
func buildPager(ns, name string, q stackQuery, changes []store.Run, next *store.Cursor) pager {
	path := "/stacks/" + ns + "/" + name
	at := func(mod func(*stackQuery)) string {
		c := q
		mod(&c)
		return withQuery(path, c.values())
	}
	var p pager
	if q.Before == nil {
		p.Live = withQuery("/fragments/stacks/"+ns+"/"+name+"/runs",
			stackQuery{Limit: q.Limit, Expand: q.Expand}.values())
	} else {
		p.Latest = at(func(c *stackQuery) { c.Before = nil })
		if len(changes) > 0 {
			// A page of only previews has no change to anchor on; Latest covers it.
			newest := &store.Cursor{At: sortTime(changes[0]), ID: changes[0].ID}
			p.Newer = at(func(c *stackQuery) { c.Before, c.After = nil, newest })
		}
	}
	if next != nil {
		p.Older = at(func(c *stackQuery) { c.Before = next })
	}
	p.Toggle = at(func(c *stackQuery) { c.Expand = !c.Expand })
	for _, n := range pageSizes {
		p.Sizes = append(p.Sizes, sizeLink{N: n, URL: at(func(c *stackQuery) { c.Limit = n }),
			Current: n == q.Limit})
	}
	return p
}
