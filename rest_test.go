package crosswork

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func body(t *testing.T, name string) func(http.ResponseWriter, *http.Request, int) {
	b := fixture(t, name)
	return func(w http.ResponseWriter, r *http.Request, n int) { fmt.Fprint(w, b) }
}

// verbCase runs one call and checks the method, path and body the server received.
func verbCase(t *testing.T, method, body string, call func(c *Client) (Res, error)) {
	t.Helper()
	f := newFake(t)
	c := f.client(t)
	_, err := call(&c)
	require.NoError(t, err)
	got := f.last()
	assert.Equal(t, method, got.Method)
	assert.Equal(t, apiPath, got.Path)
	assert.Equal(t, body, got.Body)
}

func TestClientGet(t *testing.T) {
	verbCase(t, "GET", "", func(c *Client) (Res, error) { return c.Get(apiPath) })
}
func TestClientPost(t *testing.T) {
	verbCase(t, "POST", `{"a":1}`, func(c *Client) (Res, error) { return c.Post(apiPath, `{"a":1}`) })
}
func TestClientPut(t *testing.T) {
	verbCase(t, "PUT", `{"a":2}`, func(c *Client) (Res, error) { return c.Put(apiPath, `{"a":2}`) })
}
func TestClientPatch(t *testing.T) {
	verbCase(t, "PATCH", `{"a":3}`, func(c *Client) (Res, error) { return c.Patch(apiPath, `{"a":3}`) })
}
func TestClientDelete(t *testing.T) {
	verbCase(t, "DELETE", "", func(c *Client) (Res, error) { return c.Delete(apiPath) })
}
func TestClientDeleteBody(t *testing.T) {
	b := `{"data":[{"profile":"x"}]}`
	verbCase(t, "DELETE", b, func(c *Client) (Res, error) { return c.DeleteBody(apiPath, b) })
}

func TestClientDo_RestconfErrorsParsed(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(404)
		fmt.Fprint(w, fixture(t, "restconf_errors.json"))
	}
	c := f.client(t)
	res, err := c.Get("/crosswork/proxy/nso/restconf/data/tailf-ncs:devices/device=none")
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
	ae := asAPIError(err)
	require.NotNil(t, ae)
	assert.Equal(t, "invalid-value", ae.Code)
	assert.Equal(t, "uri keypath not found", ae.RestconfErrors[0].Message)
	assert.True(t, res.Get(`ietf-restconf:errors.error.0.error-message`).Exists() || res.Raw != "", "Res keeps the body")
}

func TestClientDo_LogsNeverContainSecrets(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f := newFake(t)
	c := f.client(t)
	payload := `{"data":[{"profile":"p","user_pass":[{"user_name":"u","password":"S3cretValue!"}]}]}`
	_, err := c.Post("/crosswork/inventory/v1/credentials", payload)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, `"password":"****"`, "payloads are logged with secret members masked")
	assert.NotContains(t, out, "S3cretValue!")
	assert.NotContains(t, out, "Bearer", "the Authorization header is never logged")
	assert.NotContains(t, out, "pw", "the login password is never logged")

	buf.Reset()
	_, err = c.Post("/crosswork/inventory/v1/credentials", payload, NoLogPayload)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "user_pass", "NoLogPayload logs the request line only")
}

func TestClientNewReq_BuildErrorIsReturned(t *testing.T) {
	c, err := NewClient("https://host", "u", "p")
	require.NoError(t, err)
	_, err = c.Get("/path with\x7f bad char")
	assert.Error(t, err)
}
