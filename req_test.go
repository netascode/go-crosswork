package crosswork

import (
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
