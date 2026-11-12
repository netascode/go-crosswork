package crosswork

import (
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
)

// Crosswork pages its lists in several incompatible ways, and many list calls are POSTs, so
// Get cannot page by itself. Paginate drives a Pager
// that describes one endpoint; the adapters in pagers.go cover the styles Crosswork uses.

// maxPages stops a pager that never ends. It is a variable so tests can lower it.
var maxPages = 100000

// PageRequest is the request for one page.
type PageRequest struct {
	Method string
	Path   string
	Body   string
	Mods   []func(*Req)
}

// PageResult is a fetched page.
type PageResult struct {
	// Res is the response of the page.
	Res Res
	// Items are the items of this page, as returned by Pager.Items.
	Items []Res
	// Page is the number of the page just fetched, starting at 0.
	Page int
	// Fetched is the number of items fetched so far, including this page.
	Fetched int
}

// Pager describes how an endpoint is paged.
type Pager interface {
	// Next returns the request for the page after prev, or false when there is no further
	// page. prev is nil for the first page.
	Next(prev *PageResult) (PageRequest, bool)
	// Items returns the items of a response.
	Items(res Res) []Res
}

// Paginate fetches every page of a list and returns all the items. It stops when the pager
// says there is no further page, when a page repeats the previous one (a server that ignores
// the paging parameters would otherwise be read forever), or after maxPages pages. If a page
// fails it returns the items fetched so far together with the error.
//
// The mods are applied to every page request, e.g. Context or NoLogPayload.
//
//	items, err := client.Paginate(crosswork.NewCursorPager("/crosswork/inventory/v1/nodes/query", `{}`, 100))
func (client *Client) Paginate(p Pager, mods ...func(*Req)) ([]Res, error) {
	var all []Res
	var prev *PageResult
	var prevKey uint64
	for page := 0; ; page++ {
		if page >= maxPages {
			return all, fmt.Errorf("pagination stopped after %d pages", maxPages)
		}
		pr, ok := p.Next(prev)
		if !ok {
			return all, nil
		}
		var body io.Reader
		if pr.Body != "" {
			body = strings.NewReader(pr.Body)
		}
		mm := append(append([]func(*Req){}, pr.Mods...), mods...)
		res, err := client.Do(client.NewReq(pr.Method, pr.Path, body, mm...))
		if err != nil {
			return all, err
		}
		items := p.Items(res)
		key := itemsKey(items)
		if page > 0 && len(items) > 0 && key == prevKey {
			return all, nil // the server ignored the paging parameters and sent the same page again
		}
		all = append(all, items...)
		prev = &PageResult{Res: res, Items: items, Page: page, Fetched: len(all)}
		prevKey = key
	}
}

func itemsKey(items []Res) uint64 {
	h := fnv.New64a()
	for _, it := range items {
		h.Write([]byte(it.Raw))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// itemsAt returns the array at a gjson path of a response; an empty path means the response
// itself is the array.
func itemsAt(res Res, path string) []Res {
	if path == "" {
		if res.IsArray() {
			return res.Array()
		}
		return nil
	}
	if v := res.Get(path); v.IsArray() {
		return v.Array()
	}
	return nil
}

// readMods marks a paging POST as a read, so it may be retried after an ambiguous failure.
func readMods(method string, extra ...func(*Req)) []func(*Req) {
	if method == http.MethodPost {
		return append([]func(*Req){ReadOnly}, extra...)
	}
	return extra
}
