package crosswork

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func mkItems(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"uuid": fmt.Sprintf("u-%03d", i), "name": fmt.Sprintf("item-%d", i)}
	}
	return out
}

func names(items []Res) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Get("name").String())
	}
	return out
}

func reqBody(r *http.Request) gjson.Result {
	b, _ := io.ReadAll(r.Body)
	return gjson.ParseBytes(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Write(b)
}

// dlmServer imitates DLM nodes/query: `limit`, and `next_from` holding the uuid of the last
// item received; the response has total_count and no `data` member on an empty page.
func dlmServer(all []map[string]any) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		b := reqBody(r)
		start := 0
		if nf := b.Get("next_from").String(); nf != "" {
			for i, it := range all {
				if it["uuid"] == nf {
					start = i + 1
				}
			}
		}
		end := start + int(b.Get("limit").Int())
		if end > len(all) {
			end = len(all)
		}
		resp := map[string]any{"total_count": len(all), "result_count": len(all)}
		if end > start {
			resp["data"] = all[start:end]
		}
		writeJSON(w, resp)
	}
}

func TestClientPaginate_PagingPostsAreRetried(t *testing.T) {
	f := newFake(t)
	inner := dlmServer(mkItems(5))
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		inner(w, r, n)
	}
	c := f.client(t)
	items, err := c.Paginate(NewCursorPager("/crosswork/inventory/v1/credentials/query", ``, 10))
	require.NoError(t, err, "a 503 on a paging POST is retried because it is a read")
	assert.Len(t, items, 5)
}

// collectionServer imitates the collection service: query_options.page_size/page_token, a new
// token with every page of items, and `query_options: null` (and no items) at the end.
func collectionServer(all []map[string]any, nested bool) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		b := reqBody(r)
		opt := "query_options"
		if nested {
			opt = "query_options.pagination"
		}
		size := int(b.Get(opt + ".page_size").Int())
		start := 0
		if tok := b.Get(opt + ".page_token").String(); tok != "" {
			fmt.Sscanf(tok, "t%d", &start)
		}
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		resp := map[string]any{"result": map[string]any{"request_result": "ACCEPTED", "error": map[string]string{"error": ""}}}
		list := all[start:min(start, len(all)):min(start, len(all))]
		if start < len(all) {
			list = all[start:end]
		}
		key := "collection_job_status_list"
		if nested {
			key = "events"
		}
		resp[key] = list
		if len(list) == 0 {
			resp["query_options"] = nil
			if start > 0 { // as on the real service, running off the end of the list is a REJECTED response
				resp["result"] = map[string]any{"request_result": "REJECTED", "error": map[string]string{"error": "<nil>is not found in db"}}
			}
		} else if nested {
			resp["query_options"] = map[string]any{"pagination": map[string]any{"page_token": fmt.Sprintf("t%d", end), "page_size": size}}
		} else {
			resp["query_options"] = map[string]any{"page_token": fmt.Sprintf("t%d", end), "page_size": 100}
		}
		writeJSON(w, resp)
	}
}

func TestClientPaginate_ServerIgnoringParametersIsReadOnce(t *testing.T) {
	// some lists ignore the paging parameters and return everything whatever the page
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) { writeJSON(w, mkItems(7)) }
	c := f.client(t)
	items, err := c.Paginate(&PageNumberPager{Path: "/crosswork/performance/v1/policies", PageParam: "page", SizeParam: "pageSize", FirstPage: 1, Size: 3})
	require.NoError(t, err)
	assert.Len(t, items, 7, "no duplicates: the repeated page is dropped and the walk stops")
	assert.Equal(t, 2, f.callCount("/crosswork/performance/v1/policies"))
}

func TestClientPaginate_ReturnsItemsSoFarOnError(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if n >= 3 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"bad page"}`)
			return
		}
		listServer(mkItems(100), "offset", "limit", func(l []map[string]any) any { return l })(w, r, n)
	}
	c := f.client(t)
	items, err := c.Paginate(&OffsetPager{Path: "/crosswork/x", Limit: 10})
	require.Error(t, err)
	assert.Equal(t, 400, StatusCode(err))
	assert.Len(t, items, 20, "two good pages were fetched before the failure")
}

func TestClientPaginate_StopsEndlessPager(t *testing.T) {
	old := maxPages
	maxPages = 4
	defer func() { maxPages = old }()
	f := newFake(t)
	var n int32
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) { // every page is new, so only the guard can stop it
		writeJSON(w, []map[string]any{{"name": fmt.Sprint(atomic.AddInt32(&n, 1))}})
	}
	c := f.client(t)
	items, err := c.Paginate(&endless{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped after 4 pages")
	assert.Len(t, items, 4)
}

func TestClientPaginate_HonoursContext(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		time.Sleep(60 * time.Millisecond)
		listServer(mkItems(1000), "offset", "limit", func(l []map[string]any) any { return l })(w, r, n)
	}
	c := f.client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	bound := c.WithContext(ctx) // assign first: the methods have pointer receivers
	items, err := bound.Paginate(&OffsetPager{Path: "/crosswork/x", Limit: 10})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.NotEmpty(t, items, "pages fetched before the deadline are returned")
}
