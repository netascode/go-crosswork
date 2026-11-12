package crosswork

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchPath(t *testing.T) {
	for in, want := range map[string]string{
		"/crosswork/inventory/v1/nodes":                "/crosswork/inventory/v1/nodes",
		"/crosswork/optima-analytics//api/v1/x":        "/crosswork/optima-analytics/api/v1/x",
		"/gateway/prefix/crosswork/inventory/v1/nodes": "/crosswork/inventory/v1/nodes",
		"/restconf/data/v1/cisco-alarm:handle-alarm":   "/restconf/data/v1/cisco-alarm:handle-alarm",
		"//crosswork///proxy/nso/restconf/data/a":      "/crosswork/proxy/nso/restconf/data/a",
	} {
		u, _ := url.Parse("https://h:30603" + in)
		assert.Equal(t, want, matchPath(u), in)
	}
}

func TestIsRestconf(t *testing.T) {
	yes := []string{
		"/crosswork/nbi/topology/v3/restconf/data/ietf-network:networks",
		"/crosswork/nbi/cat-inventory/v1/restconf/operations/cat-inventory-rpc:get-all-services",
		"/crosswork/proxy/nso/restconf/data",
		"/crosswork/proxy/nso/restconf",
		"/crosswork/inventory/restconf/data/v2/resource-physical:node",
		"/restconf/data/v1/cisco-alarm:handle-alarm",
	}
	no := []string{
		"/crosswork/inventory/v1/nodes/query",
		"/crosswork/sso/v2/userInfo",
		"/crosswork/restconfig/v1/x",
		"/crosswork/inventory/v1/nodes?restconf=1",
	}
	for _, p := range yes {
		assert.True(t, isRestconf(p), p)
	}
	for _, p := range no {
		u, _ := url.Parse("https://h" + p)
		assert.False(t, isRestconf(matchPath(u)), p)
	}
}

func TestReadOnlyPost(t *testing.T) {
	for _, p := range []string{"/crosswork/inventory/v1/nodes/query", "/crosswork/inventory/v1/nodes/querybrief", "/crosswork/inventory/v1/credentials/query/", "/crosswork/ztp/v1/query/count"} {
		assert.True(t, readOnlyPost(p), p)
	}
	for _, p := range []string{"/crosswork/inventory/v1/credentials", "/crosswork/inventory/v1/nodes", "/crosswork/inventory/v1/locknodes", "/crosswork/x/queryfoo"} {
		assert.False(t, readOnlyPost(p), p)
	}
}

func TestCanRetryAmbiguous(t *testing.T) {
	mk := func(method, path string, mods ...func(*Req)) Req {
		c := Client{Url: "https://h"}
		return c.NewReq(method, path, nil, mods...)
	}
	assert.True(t, canRetryAmbiguous(mk("GET", "/crosswork/a")))
	assert.True(t, canRetryAmbiguous(mk("PUT", "/crosswork/a")))
	assert.True(t, canRetryAmbiguous(mk("POST", "/crosswork/a/query")))
	assert.True(t, canRetryAmbiguous(mk("POST", "/crosswork/a", ReadOnly)))
	assert.True(t, canRetryAmbiguous(mk("DELETE", "/crosswork/a", RetryNonIdempotent)))
	assert.False(t, canRetryAmbiguous(mk("POST", "/crosswork/a")))
	assert.False(t, canRetryAmbiguous(mk("PATCH", "/crosswork/a")))
	assert.False(t, canRetryAmbiguous(mk("DELETE", "/crosswork/a")))
}

func TestIsAuthFailure(t *testing.T) {
	login := http.Header{"Location": {"https://cnc.example.com:30603/crosswork/sso/login?client_name=swagger.json"}}
	other := http.Header{"Location": {"https://elsewhere.example.com/somewhere"}}
	assert.True(t, isAuthFailure(401, nil))
	assert.True(t, isAuthFailure(403, nil))
	assert.True(t, isAuthFailure(302, login))
	assert.True(t, isAuthFailure(307, login))
	assert.False(t, isAuthFailure(302, other), "only a redirect to the SSO login page means a dead token")
	assert.False(t, isAuthFailure(302, nil))
	assert.False(t, isAuthFailure(404, nil))
	assert.False(t, isAuthFailure(500, nil))
}

func TestClientDo_DefaultHeadersFollowPath(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	for path, want := range map[string]string{
		apiPath: "application/json",
		"/crosswork/nbi/topology/v3/restconf/data/ietf-network:networks": "application/yang-data+json",
		"/crosswork//proxy/nso//restconf/data/x":                         "application/yang-data+json",
		"/crosswork/inventory/restconf/data/v2/resource-physical:node":   "application/yang-data+json",
		"/crosswork/dg-manager/v2/hapool/query":                          "application/json",
		"/crosswork//dg-manager/v2/vdg/query":                            "application/json",
	} {
		_, err := c.Get(path)
		require.NoError(t, err)
		h := f.last().Header
		wantCT := want
		if want == "application/json" && !strings.Contains(path, "dg-manager") {
			wantCT = "application/json; charset=UTF-8"
		}
		assert.Equal(t, wantCT, h.Get("Content-Type"), path)
		assert.Equal(t, want, h.Get("Accept"), path)
	}
	_, err := c.Get(apiPath, ContentType("text/plain"), Accept("application/xml"))
	require.NoError(t, err)
	assert.Equal(t, "text/plain", f.last().Header.Get("Content-Type"))
	assert.Equal(t, "application/xml", f.last().Header.Get("Accept"))
	assert.Equal(t, "go-crosswork netascode", f.last().Header.Get("User-Agent"))
}
