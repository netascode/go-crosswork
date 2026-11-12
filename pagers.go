package crosswork

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The adapters below implement Pager for the paging styles of the Crosswork APIs.

// CursorPager pages a POST query with a cursor taken from the last item of the previous page.
// DLM works like this: the request carries `limit` and `next_from`, and the response has no
// cursor, so the client passes the `uuid` of the last item it received. The response has no
// `data` member on the last, empty page.
type CursorPager struct {
	Path string
	// Body is the base request, e.g. a filter; leave it empty for `{}`.
	Body  string
	Limit int
	// LimitField is the request member for the page size (default "limit").
	LimitField string
	// CursorField is the request member that takes the cursor (default "next_from").
	CursorField string
	// ItemCursor is the member of the last item that becomes the cursor (default "uuid").
	ItemCursor string
	// ItemsPath is where the items are in the response (default "data").
	ItemsPath string
}

// NewCursorPager returns a CursorPager with the DLM defaults.
func NewCursorPager(path, body string, limit int) *CursorPager {
	return &CursorPager{Path: path, Body: body, Limit: limit}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Items implements Pager for CursorPager.
func (c *CursorPager) Items(res Res) []Res { return itemsAt(res, orDefault(c.ItemsPath, "data")) }

// Next implements Pager for CursorPager.
func (c *CursorPager) Next(prev *PageResult) (PageRequest, bool) {
	body := orDefault(c.Body, "{}")
	body, _ = sjson.Set(body, orDefault(c.LimitField, "limit"), c.Limit)
	if prev != nil {
		if len(prev.Items) == 0 || len(prev.Items) < c.Limit {
			return PageRequest{}, false
		}
		cur := prev.Items[len(prev.Items)-1].Get(orDefault(c.ItemCursor, "uuid"))
		if cur.String() == "" {
			return PageRequest{}, false
		}
		body, _ = sjson.Set(body, orDefault(c.CursorField, "next_from"), cur.String())
	}
	return PageRequest{Method: http.MethodPost, Path: c.Path, Body: body, Mods: readMods(http.MethodPost)}, true
}

// TokenPager pages a POST query with an opaque page token. The collection service nests the
// options directly under `query_options` (page_size, page_token); the platform services nest
// them one level deeper, under `query_options.pagination`. The response carries the token of
// the next page in the same place, and the last page has no `query_options` (collection) or
// an empty token. The collection service ends the walk with a REJECTED response, which is accepted
// as the end of the list; the platform lists end with an empty page that repeats the token.
type TokenPager struct {
	Path string
	// Body is the base request; leave it empty for `{}`.
	Body     string
	PageSize int
	// OptionsPath is where the paging options live (default "query_options").
	OptionsPath string
	// ItemsPath is where the items are in the response, e.g. "collection_job_status_list".
	ItemsPath string
}

// Items implements Pager for TokenPager.
func (t *TokenPager) Items(res Res) []Res { return itemsAt(res, t.ItemsPath) }

// Next implements Pager for TokenPager.
func (t *TokenPager) Next(prev *PageResult) (PageRequest, bool) {
	opts := orDefault(t.OptionsPath, "query_options")
	body := orDefault(t.Body, "{}")
	body, _ = sjson.Set(body, opts+".page_size", t.PageSize)
	if prev != nil {
		tok := prev.Res.Get(opts + ".page_token")
		if len(prev.Items) == 0 || tok.String() == "" {
			return PageRequest{}, false
		}
		body, _ = sjson.Set(body, opts+".page_token", tok.String())
	}
	mods := readMods(http.MethodPost)
	if prev != nil {
		mods = append(mods, ResultCheck(tokenWalkCheck(t.ItemsPath)))
	}
	return PageRequest{Method: http.MethodPost, Path: t.Path, Body: body, Mods: mods}, true
}

// tokenWalkCheck is the result check for pages after the first. The collection service ends a
// token walk with a response that has an empty list, `query_options: null` and a REJECTED
// result ("is not found in db"). That shape is the end of the list,
// not a failure, so it is accepted here; the request is still rejected if it is the first page,
// and any other failure shape is still an error.
func tokenWalkCheck(itemsPath string) func(Res) error {
	return func(res Res) error {
		if qo := res.Get("query_options"); qo.Exists() && qo.Type == gjson.Null &&
			len(itemsAt(res, itemsPath)) == 0 && res.Get("result.request_result").String() == "REJECTED" {
			return nil
		}
		if msg, ok := resultFailure(res); ok {
			return errors.New(msg)
		}
		return nil
	}
}

// OffsetPager pages a GET with `offset` and `limit` query parameters, as the RESTCONF list
// reads of the topology service do. The offset advances by the number of items received and
// the last page is short.
type OffsetPager struct {
	Path        string
	Limit       int
	OffsetParam string // default "offset"
	LimitParam  string // default "limit"
	// ItemsPath is the path of the list in the response, e.g.
	// "cisco-crosswork-segment-routing-policy:sr-policies.policy".
	ItemsPath string
}

// Items implements Pager for OffsetPager.
func (o *OffsetPager) Items(res Res) []Res { return itemsAt(res, o.ItemsPath) }

// Next implements Pager for OffsetPager.
func (o *OffsetPager) Next(prev *PageResult) (PageRequest, bool) {
	offset := 0
	if prev != nil {
		if len(prev.Items) == 0 || len(prev.Items) < o.Limit {
			return PageRequest{}, false
		}
		offset = prev.Fetched
	}
	return PageRequest{Method: http.MethodGet, Path: o.Path, Mods: []func(*Req){
		Query(orDefault(o.OffsetParam, "offset"), strconv.Itoa(offset)),
		Query(orDefault(o.LimitParam, "limit"), strconv.Itoa(o.Limit)),
	}}, true
}

// HeaderPager pages a RESTCONF RPC whose input has a `collection-header` with `offset` and
// `limit` (the Active Topology service inventory RPCs). The offset advances by the number of
// items received.
type HeaderPager struct {
	Path string
	// Body is the base RPC input.
	Body  string
	Limit int
	// HeaderPath is the path of the collection-header in the request, e.g.
	// "cat-inventory-rpc:input.cat-inventory-rpc:get-all-services-request.collection-header".
	HeaderPath string
	// ItemsPath is the path of the item list in the response, e.g.
	// "cat-inventory-rpc:output.get-all-services-response.collection-data.service-info".
	ItemsPath string
}

// Items implements Pager for HeaderPager.
func (h *HeaderPager) Items(res Res) []Res { return itemsAt(res, h.ItemsPath) }

// Next implements Pager for HeaderPager.
func (h *HeaderPager) Next(prev *PageResult) (PageRequest, bool) {
	offset := 0
	if prev != nil {
		if len(prev.Items) == 0 || len(prev.Items) < h.Limit {
			return PageRequest{}, false
		}
		offset = prev.Fetched
	}
	body := orDefault(h.Body, "{}")
	body, _ = sjson.Set(body, h.HeaderPath+".offset", offset)
	body, _ = sjson.Set(body, h.HeaderPath+".limit", h.Limit)
	return PageRequest{Method: http.MethodPost, Path: h.Path, Body: body, Mods: readMods(http.MethodPost)}, true
}

// PageNumberPager pages a GET with a page number and a page size, whose parameter names differ
// by service: page/pageSize (EMF performance), PageNum/PageSize (ZTP config), and so on.
// For example /performance/v1/policies/inventory-devices: the page number is 1-based and page=0 is a
// 500. Some lists ignore both parameters, which Paginate copes with by stopping when a page repeats.
type PageNumberPager struct {
	Path      string
	PageParam string
	SizeParam string
	// FirstPage is the number of the first page (0 or 1).
	FirstPage int
	Size      int
	// ItemsPath is where the items are in the response ("content" for a Spring page); an empty
	// path means the response is the array.
	ItemsPath string
}

// Items implements Pager for PageNumberPager.
func (p *PageNumberPager) Items(res Res) []Res { return itemsAt(res, p.ItemsPath) }

// Next implements Pager for PageNumberPager.
func (p *PageNumberPager) Next(prev *PageResult) (PageRequest, bool) {
	page := p.FirstPage
	if prev != nil {
		if len(prev.Items) == 0 || len(prev.Items) < p.Size {
			return PageRequest{}, false
		}
		page += prev.Page + 1
	}
	return PageRequest{Method: http.MethodGet, Path: p.Path, Mods: []func(*Req){
		Query(p.PageParam, strconv.Itoa(page)),
		Query(p.SizeParam, strconv.Itoa(p.Size)),
	}}, true
}

// BodyPager pages a POST query whose request and response both carry a `pagination` object, the style of
// the Workflow Manager Solutions services (Golden Configuration, MOP): the request sets
// `pagination.pageNumber` and `pagination.pageSize`, and the response repeats the object, the Golden
// Configuration lists with `totalPages` and `last`. The first page is number 0 for Golden Configuration
// and 1 for the MOP lists, so FirstPage is a field.
type BodyPager struct {
	Path string
	// Body is the base request, e.g. a filter; leave it empty for `{}`.
	Body string
	Size int
	// FirstPage is the number of the first page (0 or 1).
	FirstPage int
	// PaginationPath is where the paging object is, in the request and in the response (default "pagination").
	PaginationPath string
	// ItemsPath is where the items are in the response (default "data").
	ItemsPath string
}

// Items implements Pager for BodyPager.
func (b *BodyPager) Items(res Res) []Res { return itemsAt(res, orDefault(b.ItemsPath, "data")) }

// Next implements Pager for BodyPager.
func (b *BodyPager) Next(prev *PageResult) (PageRequest, bool) {
	pp := orDefault(b.PaginationPath, "pagination")
	page := b.FirstPage
	if prev != nil {
		if len(prev.Items) == 0 || len(prev.Items) < b.Size || prev.Res.Get(pp+".last").Bool() {
			return PageRequest{}, false
		}
		page += prev.Page + 1
	}
	body := orDefault(b.Body, "{}")
	body, _ = sjson.Set(body, pp+".pageNumber", page)
	body, _ = sjson.Set(body, pp+".pageSize", b.Size)
	return PageRequest{Method: http.MethodPost, Path: b.Path, Body: body, Mods: readMods(http.MethodPost)}, true
}

// FilterDataPager pages a POST query of the inventory service whose request carries `filterData` with
// `PageSize`, `PageNum` (the first page is 0) and an optional `Criteria`, such as the node query
// `select uuid from robotnodedata where dg_uuid=<id>` that lists the devices of a data gateway. The last page
// is short; a page that is exactly full is followed by one more request that answers an empty list.
type FilterDataPager struct {
	Path string
	// Filter is the `filter` member of the request as JSON object text; empty for `{}`.
	Filter string
	// Criteria is the `filterData.Criteria` text; empty for none.
	Criteria string
	// Size is the page size; 200 when zero or negative.
	Size int
	// ItemsPath is where the items are in the response (default "data").
	ItemsPath string
}

// Items implements Pager for FilterDataPager.
func (f *FilterDataPager) Items(res Res) []Res { return itemsAt(res, orDefault(f.ItemsPath, "data")) }

// Next implements Pager for FilterDataPager.
func (f *FilterDataPager) Next(prev *PageResult) (PageRequest, bool) {
	size := f.Size
	if size <= 0 {
		size = 200
	}
	page := 0
	if prev != nil {
		if len(prev.Items) == 0 || len(prev.Items) < size {
			return PageRequest{}, false
		}
		page = prev.Page + 1
	}
	body, _ := sjson.SetRaw("{}", "filter", orDefault(f.Filter, "{}"))
	body, _ = sjson.Set(body, "filterData.PageSize", size)
	body, _ = sjson.Set(body, "filterData.PageNum", page)
	if f.Criteria != "" {
		body, _ = sjson.Set(body, "filterData.Criteria", f.Criteria)
	}
	return PageRequest{Method: http.MethodPost, Path: f.Path, Body: body, Mods: readMods(http.MethodPost)}, true
}
