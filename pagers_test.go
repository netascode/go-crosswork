package crosswork

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// listServer imitates a GET list with offset/limit (the topology RESTCONF reads): the list sits
// under a RESTCONF container, and the last page is short.
func listServer(all []map[string]any, offsetParam, limitParam string, wrap func([]map[string]any) any) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		q := r.URL.Query()
		off, _ := strconv.Atoi(q.Get(offsetParam))
		lim, _ := strconv.Atoi(q.Get(limitParam))
		if off > len(all) {
			off = len(all)
		}
		end := off + lim
		if end > len(all) || lim == 0 {
			end = len(all)
		}
		writeJSON(w, wrap(all[off:end]))
	}
}

func TestClientPaginate_OffsetPager(t *testing.T) {
	for _, total := range []int{44, 40, 5, 0} {
		f := newFake(t)
		f.api = listServer(mkItems(total), "offset", "limit", func(l []map[string]any) any {
			return map[string]any{"cisco-crosswork-segment-routing-policy:sr-policies": map[string]any{"policy": l}}
		})
		c := f.client(t)
		items, err := c.Paginate(&OffsetPager{Path: "/crosswork/nbi/topology/v3/restconf/data/cisco-crosswork-segment-routing-policy:sr-policies",
			Limit: 10, ItemsPath: "cisco-crosswork-segment-routing-policy:sr-policies.policy"})
		require.NoError(t, err, total)
		assert.Len(t, items, total)
		assert.Equal(t, "application/yang-data+json", f.last().Header.Get("Accept"), "RESTCONF paths keep their headers")
	}
	f := newFake(t)
	f.api = listServer(mkItems(12), "off", "max", func(l []map[string]any) any { return l })
	c := f.client(t)
	items, err := c.Paginate(&OffsetPager{Path: "/crosswork/x", Limit: 5, OffsetParam: "off", LimitParam: "max"})
	require.NoError(t, err)
	assert.Len(t, items, 12, "parameter names are configurable; an array response needs no ItemsPath")
}

func TestClientPaginate_HeaderPager(t *testing.T) {
	const hdr = "cat-inventory-rpc:input.cat-inventory-rpc:get-all-services-request.collection-header"
	for _, total := range []int{36, 7, 0} {
		f := newFake(t)
		all := mkItems(total)
		f.api = func(w http.ResponseWriter, r *http.Request, n int) {
			b := reqBody(r)
			off, lim := int(b.Get(hdr+".offset").Int()), int(b.Get(hdr+".limit").Int())
			end := off + lim
			if end > len(all) {
				end = len(all)
			}
			if off > len(all) {
				off = len(all)
			}
			page := all[off:end]
			writeJSON(w, map[string]any{"cat-inventory-rpc:output": map[string]any{"get-all-services-response": map[string]any{
				"collection-header": map[string]any{"offset": off, "count": len(page)},
				"collection-data":   map[string]any{"service-info": page}}}})
		}
		c := f.client(t)
		items, err := c.Paginate(&HeaderPager{Path: "/crosswork/nbi/cat-inventory/v1/restconf/operations/cat-inventory-rpc:get-all-services",
			Body: `{}`, Limit: 5, HeaderPath: hdr, ItemsPath: "cat-inventory-rpc:output.get-all-services-response.collection-data.service-info"})
		require.NoError(t, err, total)
		assert.Len(t, items, total)
	}
}

func TestClientPaginate_PageNumberPager(t *testing.T) {
	for _, first := range []int{0, 1} {
		f := newFake(t)
		all := mkItems(7)
		f.api = func(w http.ResponseWriter, r *http.Request, n int) {
			q := r.URL.Query()
			page, _ := strconv.Atoi(q.Get("PageNum"))
			size, _ := strconv.Atoi(q.Get("PageSize"))
			start := (page - first) * size
			if start > len(all) {
				start = len(all)
			}
			end := min(start+size, len(all))
			writeJSON(w, map[string]any{"content": all[start:end], "pageNumber": page, "pageSize": end - start})
		}
		c := f.client(t)
		items, err := c.Paginate(&PageNumberPager{Path: "/crosswork/ztpconfig/v1/config", PageParam: "PageNum", SizeParam: "PageSize", FirstPage: first, Size: 3, ItemsPath: "content"})
		require.NoError(t, err)
		assert.Len(t, items, 7, "first page %d", first)
		assert.Equal(t, "7", fmt.Sprint(len(items)))
	}
}

type endless struct{ n int32 }

func (e *endless) Items(res Res) []Res { return res.Array() }
func (e *endless) Next(prev *PageResult) (PageRequest, bool) {
	return PageRequest{Method: http.MethodGet, Path: "/crosswork/x"}, true
}

func TestClientPaginate_CursorPager(t *testing.T) {
	for _, total := range []int{25, 20, 3, 0} {
		t.Run(fmt.Sprintf("%d items", total), func(t *testing.T) {
			f := newFake(t)
			f.api = dlmServer(mkItems(total))
			c := f.client(t)
			items, err := c.Paginate(NewCursorPager("/crosswork/inventory/v1/nodes/query", `{"filter":{"x":1}}`, 10))
			require.NoError(t, err)
			assert.Len(t, items, total)
			seen := map[string]bool{}
			for _, n := range names(items) {
				assert.False(t, seen[n], "duplicate "+n)
				seen[n] = true
			}
			assert.Equal(t, int64(1), gjson.Parse(f.last().Body).Get("filter.x").Int(), "the base request (filter) is kept on every page")
		})
	}
}

func TestClientPaginate_CursorPagerRequests(t *testing.T) {
	f := newFake(t)
	f.api = dlmServer(mkItems(25))
	c := f.client(t)
	_, err := c.Paginate(NewCursorPager("/crosswork/inventory/v1/nodes/query", ``, 10))
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	var bodies []gjson.Result
	for _, r := range f.reqs {
		if r.Path == "/crosswork/inventory/v1/nodes/query" {
			bodies = append(bodies, gjson.Parse(r.Body))
		}
	}
	require.Len(t, bodies, 3, "10 + 10 + 5: the short page ends the walk")
	assert.False(t, bodies[0].Get("next_from").Exists())
	assert.Equal(t, "u-009", bodies[1].Get("next_from").String(), "the cursor is the uuid of the last item received")
	assert.Equal(t, "u-019", bodies[2].Get("next_from").String())
	assert.Equal(t, int64(10), bodies[2].Get("limit").Int())
}

func TestClientPaginate_TokenPager(t *testing.T) {
	for _, tc := range []struct {
		name   string
		total  int
		nested bool
	}{{"collection, partial last page", 12, false}, {"collection, exact multiple", 10, false}, {"platform nesting", 7, true}, {"empty", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.api = collectionServer(mkItems(tc.total), tc.nested)
			c := f.client(t)
			p := &TokenPager{Path: "/crosswork/collection/v1/collectionjob/summary/query", PageSize: 5, ItemsPath: "collection_job_status_list"}
			if tc.nested {
				p = &TokenPager{Path: "/crosswork/platform/v2/georedundancy/events/query", PageSize: 5, OptionsPath: "query_options.pagination", ItemsPath: "events"}
			}
			items, err := c.Paginate(p)
			require.NoError(t, err)
			assert.Len(t, items, tc.total)
			assert.Equal(t, 5, int(gjson.Parse(f.reqs[len(f.reqs)-1].Body).Get(orDefault(p.OptionsPath, "query_options")+".page_size").Int()))
		})
	}
}

func TestClientPaginate_TokenPagerEndMarkerAfterFirstPage(t *testing.T) {
	// the same REJECTED, empty, null-options body on the FIRST page is a real failure
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, map[string]any{"collection_job_status_list": []any{}, "query_options": nil, "result": map[string]any{"request_result": "REJECTED", "error": map[string]string{"error": "<nil>is not found in db"}}})
	}
	c := f.client(t)
	_, err := c.Paginate(&TokenPager{Path: "/crosswork/collection/v1/collectionjob/summary/query", PageSize: 5, ItemsPath: "collection_job_status_list"})
	require.Error(t, err, "a rejected first page is an error, not an empty list")
	assert.Contains(t, err.Error(), "not found in db")

	// any other failure shape after the first page is still an error
	f = newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			writeJSON(w, map[string]any{"collection_job_status_list": mkItems(5), "query_options": map[string]any{"page_token": "t5"}, "result": map[string]any{"request_result": "ACCEPTED"}})
			return
		}
		writeJSON(w, map[string]any{"collection_job_status_list": []any{}, "query_options": map[string]any{"page_token": "x"}, "result": map[string]any{"request_result": "REJECTED", "error": map[string]string{"error": "boom"}}})
	}
	c = f.client(t)
	items, err := c.Paginate(&TokenPager{Path: "/crosswork/collection/v1/collectionjob/summary/query", PageSize: 5, ItemsPath: "collection_job_status_list"})
	require.Error(t, err, "REJECTED with a live page token is a failure, not the end marker")
	assert.Len(t, items, 5)
}

// bodyPageServer imitates a Golden Configuration list: the page object sits under pagination in the request and
// in the response, the response says `last`, and page numbers start at first.
func bodyPageServer(all []map[string]any, first int) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		b := reqBody(r)
		page := int(b.Get("pagination.pageNumber").Int()) - first
		size := int(b.Get("pagination.pageSize").Int())
		start := page * size
		if start > len(all) {
			start = len(all)
		}
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		pages := (len(all) + size - 1) / size
		writeJSON(w, map[string]any{"data": all[start:end], "pagination": map[string]any{
			"pageNumber": page + first, "pageSize": size, "total": len(all), "totalPages": pages, "last": page+1 >= pages}})
	}
}

func TestBodyPager(t *testing.T) {
	for _, first := range []int{0, 1} {
		for _, total := range []int{25, 20, 3, 0} {
			t.Run(fmt.Sprintf("first %d, %d items", first, total), func(t *testing.T) {
				f := newFake(t)
				f.api = bodyPageServer(mkItems(total), first)
				c := f.client(t)
				items, err := c.Paginate(&BodyPager{Path: "/crosswork/cwms/gc/v1/templates/query", Body: `{"name":"x"}`, Size: 10, FirstPage: first})
				require.NoError(t, err)
				assert.Len(t, items, total)
				assert.Equal(t, "x", gjson.Parse(f.last().Body).Get("name").String(), "the base request is kept on every page")
				assert.Equal(t, int64(10), gjson.Parse(f.last().Body).Get("pagination.pageSize").Int())
			})
		}
	}
}

func filterDataPageServer(all []map[string]any) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		b := reqBody(r)
		page := int(b.Get("filterData.PageNum").Int())
		size := int(b.Get("filterData.PageSize").Int())
		start := page * size
		if start > len(all) {
			start = len(all)
		}
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		writeJSON(w, map[string]any{"data": all[start:end], "total_count": len(all), "result_count": end - start})
	}
}

func TestFilterDataPager(t *testing.T) {
	const path = "/crosswork/inventory/v1/nodes/query"
	for _, total := range []int{25, 20, 3, 0} {
		t.Run(fmt.Sprintf("%d items", total), func(t *testing.T) {
			f := newFake(t)
			f.api = filterDataPageServer(mkItems(total))
			c := f.client(t)
			items, err := c.Paginate(&FilterDataPager{Path: path, Filter: `{"inventory_id":"x"}`,
				Criteria: "select uuid from robotnodedata where dg_uuid=g1", Size: 10})
			require.NoError(t, err)
			assert.Len(t, items, total)
			last := gjson.Parse(f.last().Body)
			assert.Equal(t, "x", last.Get("filter.inventory_id").String(), "the filter is kept on every page")
			assert.Equal(t, "select uuid from robotnodedata where dg_uuid=g1", last.Get("filterData.Criteria").String())
			assert.Equal(t, int64(10), last.Get("filterData.PageSize").Int())
			// a full last page costs one more request that answers an empty list
			wantCalls := total/10 + 1
			assert.Equal(t, wantCalls, f.callCount(path))
		})
	}
}

func TestFilterDataPager_DefaultsAndRecordedBody(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		_, _ = w.Write([]byte(fixture(t, "nodes_filterdata_page.json")))
	}
	c := f.client(t)
	items, err := c.Paginate(&FilterDataPager{Path: "/crosswork/inventory/v1/nodes/query"})
	require.NoError(t, err)
	require.Len(t, items, 2, "a short page ends the walk")
	assert.Equal(t, "cwtest-a", items[0].Get("inventory_id").String())
	assert.Equal(t, "cw-dg-pool-1-1", items[1].Get("dg_name").String())
	body := gjson.Parse(f.last().Body)
	assert.Equal(t, int64(200), body.Get("filterData.PageSize").Int(), "the default page size")
	assert.Equal(t, int64(0), body.Get("filterData.PageNum").Int())
	assert.False(t, body.Get("filterData.Criteria").Exists(), "no criteria unless one is given")
	assert.Equal(t, "{}", body.Get("filter").Raw)
}
