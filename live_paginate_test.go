package crosswork

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ids returns the sorted identity of each item. Live items carry status fields (uptime,
// counters) that change between two calls, so identity is what must match, not the whole item.
// An empty path means the whole item.
func ids(items []Res, path string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		if path == "" {
			out[i] = it.Raw
		} else {
			out[i] = it.Get(path).Raw
		}
	}
	sort.Strings(out)
	return out
}

// Read-only. Each test walks a real list in small pages and the result must be the same set
// of items as one big call (the order can differ between a limited and an unlimited call).

func TestLiveClientPaginate_CursorNodes(t *testing.T) {
	c := liveClient(t)
	const path = "/crosswork/inventory/v1/nodes/query"
	one, err := c.Post(path, `{"limit":1000}`, ReadOnly)
	require.NoError(t, err)
	paged, err := c.Paginate(NewCursorPager(path, ``, 7))
	require.NoError(t, err)
	assert.Equal(t, ids(itemsAt(one, "data"), "uuid"), ids(paged, "uuid"))
	t.Logf("%d nodes", len(paged))
}

func TestLiveClientPaginate_TokenCollectionJobs(t *testing.T) {
	c := liveClient(t)
	const path = "/crosswork/collection/v1/collectionjob/summary/query"
	one, err := c.Post(path, `{"query_options":{"page_size":1000}}`, ReadOnly)
	require.NoError(t, err)
	paged, err := c.Paginate(&TokenPager{Path: path, PageSize: 4, ItemsPath: "collection_job_status_list"})
	require.NoError(t, err)
	assert.Equal(t, ids(itemsAt(one, "collection_job_status_list"), "application_context"), ids(paged, "application_context"))
	t.Logf("%d collection jobs", len(paged))
}

func TestLiveClientPaginate_PageNumberInventoryDevices(t *testing.T) {
	c := liveClient(t)
	// page is 1-based here (page=0 is a 500), the answer is {"data":[...],"total_count":N}
	const path = "/crosswork/performance/v1/policies/inventory-devices"
	one, err := c.Get(path)
	if err != nil {
		t.Skipf("the performance service is not available here: %v", err)
	}
	paged, err := c.Paginate(&PageNumberPager{Path: path, PageParam: "page", SizeParam: "pageSize", FirstPage: 1, Size: 10, ItemsPath: "data"})
	require.NoError(t, err)
	assert.Equal(t, ids(itemsAt(one, "data"), "hostName"), ids(paged, "hostName"))
	assert.Equal(t, int(one.Get("total_count").Int()), len(paged))
	t.Logf("%d inventory devices in pages of 10", len(paged))
}

func TestLiveClientPaginate_TokenNestedApplications(t *testing.T) {
	c := liveClient(t)
	const path = "/crosswork/platform/v2/capp/applicationsummary/query"
	one, err := c.Post(path, `{}`, ReadOnly)
	require.NoError(t, err)
	paged, err := c.Paginate(&TokenPager{Path: path, PageSize: 3, OptionsPath: "query_options.pagination", ItemsPath: "application_summary_list"})
	require.NoError(t, err)
	assert.Equal(t, ids(itemsAt(one, "application_summary_list"), ""), ids(paged, ""))
	t.Logf("%d applications in pages of 3", len(paged))
}

func TestLiveClientPaginate_OffsetSRPolicies(t *testing.T) {
	c := liveClient(t)
	const path = "/crosswork/nbi/topology/v3/restconf/data/cisco-crosswork-segment-routing-policy:sr-policies"
	const items = "cisco-crosswork-segment-routing-policy:sr-policies.policy"
	one, err := c.Get(path)
	require.NoError(t, err)
	paged, err := c.Paginate(&OffsetPager{Path: path, Limit: 7, ItemsPath: items})
	require.NoError(t, err)
	assert.Equal(t, ids(itemsAt(one, items), ""), ids(paged, ""))
	t.Logf("%d SR policies", len(paged))
}

func TestLiveClientPaginate_HeaderServices(t *testing.T) {
	c := liveClient(t)
	const path = "/crosswork/nbi/cat-inventory/v1/restconf/operations/cat-inventory-rpc:get-all-services"
	const hdr = "cat-inventory-rpc:input.cat-inventory-rpc:get-all-services-request.collection-header"
	const items = "cat-inventory-rpc:output.get-all-services-response.collection-data.service-info"
	one, err := c.Post(path, Body{}.SetRaw(hdr, `{}`).Str, ReadOnly)
	if err != nil {
		t.Skipf("the service inventory RPC is not available here: %v", err)
	}
	paged, err := c.Paginate(&HeaderPager{Path: path, Body: `{}`, Limit: 5, HeaderPath: hdr, ItemsPath: items})
	require.NoError(t, err)
	assert.Equal(t, ids(itemsAt(one, items), "service-name"), ids(paged, "service-name"))
	t.Logf("%d services", len(paged))
}
