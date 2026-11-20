package crosswork

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBodySet(t *testing.T) {
	b := Body{}.
		Set("name", "r1").
		Set("port", 22). // typed values keep their type
		Set("enabled", true).
		SetRaw("tags", `["a","b"]`).
		Set("nested.key", "v")
	assert.JSONEq(t, `{"name":"r1","port":22,"enabled":true,"tags":["a","b"],"nested":{"key":"v"}}`, b.Str)
}

func TestBodyRes(t *testing.T) {
	assert.Equal(t, int64(22), Body{}.Set("port", 22).Res().Get("port").Int())
}

func TestBodyDelete(t *testing.T) {
	b := Body{}.Set("name", "r1").Set("nested.key", "v")
	assert.JSONEq(t, `{"name":"r1"}`, b.Delete("nested").Str)
	assert.JSONEq(t, `{}`, Body{Str: `{"x":1}`}.Delete("x").Str)
}

func TestClientNewReq_Mods(t *testing.T) {
	c, _ := NewClient("https://h:30603", "u", "p")
	req := c.NewReq("POST", "/crosswork/x", nil, Timeout(7), UseMutex, NoLogPayload, ReadOnly, RetryNonIdempotent, NoResultCheck)
	assert.Equal(t, 7*time.Second, req.Timeout)
	assert.True(t, req.UseMutex)
	assert.False(t, req.LogPayload)
	assert.True(t, req.ReadOnly)
	assert.True(t, req.RetryNonIdempotent)
	assert.True(t, req.noResultCheck)
	assert.Equal(t, "https://h:30603/crosswork/x", req.HttpReq.URL.String())
}

func TestClientNewReq_LogsPayloadByDefault(t *testing.T) {
	c, _ := NewClient("https://h:30603", "u", "p")
	assert.True(t, c.NewReq("GET", "/crosswork/x", nil).LogPayload, "payloads are logged unless NoLogPayload is set")
}

func TestQuery(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	_, err := c.Get("/crosswork/x/restconf/data/y", Query("content", "config"), Query("depth", "1"))
	require.NoError(t, err)
	assert.Equal(t, "content=config&depth=1", f.last().Query)
}

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

func TestRestartsLogin(t *testing.T) {
	for path, want := range map[string]bool{
		"/crosswork/authconfig/v1/remote/tacacs":                true,
		"/crosswork/authconfig/v1/remote/ldap":                  true,
		"/crosswork/authconfig/v1/remote/radius":                true,
		"/crosswork/authconfig/v2/remote/servers":               true,
		"/crosswork/authconfig/v1/sso/idp/services":             true,
		"/gateway/prefix/crosswork/authconfig/v1/remote/tacacs": true,
		"/crosswork/authconfig/v1/clientauth":                   false,
		"/crosswork/authconfig/v1/sso/idp/config":               false,
		"/crosswork/aaa/v1/sessionconfig":                       false,
		"/crosswork/aaa/v1/passwordPolicyConfig":                false,
		"/crosswork/inventory/v1/nodes":                         false,
	} {
		assert.Equal(t, want, restartsLogin(matchPath(&url.URL{Path: path})), path)
	}
}
