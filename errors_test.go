package crosswork

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIErrorError(t *testing.T) {
	err := &APIError{StatusCode: 404, Message: "uri keypath not found"}
	assert.Equal(t, "HTTP Request failed: StatusCode 404: uri keypath not found", err.Error())
	assert.Equal(t, "HTTP Request failed: StatusCode 500", (&APIError{StatusCode: 500}).Error())
	// providers that match on the error text keep working
	assert.Contains(t, err.Error(), "StatusCode 404")
}

func TestIsNotFound(t *testing.T) {
	assert.True(t, IsNotFound(&APIError{StatusCode: 404}))
	assert.True(t, IsNotFound(&APIError{StatusCode: 409, RestconfErrors: []RestconfError{{Tag: "data-missing"}}}))
	assert.False(t, IsNotFound(&APIError{StatusCode: 409, RestconfErrors: []RestconfError{{Tag: "data-exists"}}}))
	assert.False(t, IsNotFound(errors.New("StatusCode 404")), "plain errors are never classified from their text")
	assert.True(t, IsNotFound(&APIError{StatusCode: 200, Code: "GROUP_NOT_EXIST"}))
	assert.False(t, IsNotFound(&APIError{StatusCode: 200, Code: "DUPLICATED_DEVICE_ACCESS_GROUP_NAME"}))
	assert.False(t, IsNotFound(&APIError{StatusCode: 200, Message: "GROUP_NOT_EXIST"}), "the message is not searched")
	assert.False(t, IsNotFound(nil))
}

func TestIsConflict(t *testing.T) {
	assert.True(t, IsConflict(&APIError{StatusCode: 409, RestconfErrors: []RestconfError{{Tag: "data-exists"}}}))
	assert.True(t, IsConflict(&APIError{StatusCode: 409}))
	// a DLM "already exists" arrives as a 200 job failure and is deliberately not a conflict
	assert.False(t, IsConflict(&APIError{StatusCode: 200, Message: "The new credential profile x already exists"}))
}

func TestIsUnauthorized(t *testing.T) {
	assert.True(t, IsUnauthorized(fmt.Errorf("read failed: %w", &APIError{StatusCode: 403})))
	assert.True(t, IsUnauthorized(&APIError{StatusCode: 401}))
	assert.False(t, IsUnauthorized(&APIError{StatusCode: 404}))
}

func TestIsSessionLimit(t *testing.T) {
	login := &APIError{StatusCode: 503, URL: "https://cw.example" + ticketPath, Message: "Per user session limit reached"}
	assert.True(t, IsSessionLimit(login))
	assert.True(t, IsSessionLimit(fmt.Errorf("login: %w", login)))
	// a 503 from any other endpoint is an ordinary outage
	assert.False(t, IsSessionLimit(&APIError{StatusCode: 503, URL: "https://cw.example/crosswork/inventory/v1/credentials"}))
	assert.False(t, IsSessionLimit(&APIError{StatusCode: 401, URL: "https://cw.example" + ticketPath}))
	assert.False(t, IsSessionLimit(nil))
}

func TestStatusCode(t *testing.T) {
	assert.Equal(t, 403, StatusCode(fmt.Errorf("read failed: %w", &APIError{StatusCode: 403})))
	assert.Equal(t, 0, StatusCode(nil))
	assert.Equal(t, 0, StatusCode(errors.New("x")))
}

func TestCodeFromMessage(t *testing.T) {
	assert.Equal(t, "ErrMissingProfileName", codeFromMessage("[ErrMissingProfileName]:  The credential profile name is missing."))
	assert.Equal(t, "", codeFromMessage("The new credential profile x already exists"))
	assert.Equal(t, "", codeFromMessage("see [ErrFoo] in the middle"))
}

func TestEntryErrorsOverHTTP(t *testing.T) {
	serve := func(body string) *fakeCW {
		f := newFake(t)
		f.api = func(w http.ResponseWriter, r *http.Request, n int) {
			w.WriteHeader(http.StatusMultiStatus)
			w.Write([]byte(body))
		}
		return f
	}
	c := serve(fixture(t, "cwms_entries_not_found.json")).client(t)
	_, err := c.Delete("/crosswork/cwms/gc/v1/applications/x")
	require.Error(t, err)
	assert.True(t, IsNotFound(err), "every failed entry is a 404")
	assert.False(t, IsConflict(err))
	assert.Equal(t, 207, StatusCode(err))

	c = serve(fixture(t, "cwms_entries_failed.json")).client(t)
	_, err = c.Delete("/crosswork/cwms/gc/v1/templates")
	require.Error(t, err)
	assert.True(t, IsConflict(err))
	assert.False(t, IsNotFound(err))

	c = serve(fixture(t, "cwms_entries_mixed.json")).client(t)
	_, err = c.Delete("/crosswork/cwms/gc/v1/applications/x")
	require.Error(t, err)
	assert.False(t, IsNotFound(err), "one failed entry is not a 404")
	assert.True(t, IsConflict(err), "one failed entry is a 409")
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, []int{404, 409}, ae.EntryStatus)

	c = serve(fixture(t, "cwms_entries_ok.json")).client(t)
	res, err := c.Delete("/crosswork/cwms/gc/v1/templates")
	require.NoError(t, err)
	assert.Equal(t, "Template deleted", res.Get("0.message").String())
}

func TestDataGatewayManagerErrors(t *testing.T) {
	for name, want := range map[string]string{
		"dg_pool_error.json":         "does not match the number of virtual IPs",
		"dg_content_type_error.json": "received unexpected Content-Type",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.api = func(w http.ResponseWriter, r *http.Request, n int) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(fixture(t, name)))
			}
			c := f.client(t)
			_, err := c.Put("/crosswork/dg-manager/v2/hapool", `{"pools":[]}`)
			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
			assert.Contains(t, apiErr.Message, want, "the controller's text is returned unchanged")
		})
	}
}
