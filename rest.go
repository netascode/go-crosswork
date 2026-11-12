package crosswork

import (
	"net/http"
	"strings"
)

// Get makes a GET request and returns a gjson result.
// The path is the full path, e.g. /crosswork/inventory/v1/nodes/count.
func (client *Client) Get(path string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodGet, path, nil, mods...))
}

// Post makes a POST request and returns a gjson result.
// Hint: Use the Body struct to easily create POST body data.
func (client *Client) Post(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodPost, path, strings.NewReader(data), mods...))
}

// Put makes a PUT request and returns a gjson result.
func (client *Client) Put(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodPut, path, strings.NewReader(data), mods...))
}

// Patch makes a PATCH request and returns a gjson result.
func (client *Client) Patch(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodPatch, path, strings.NewReader(data), mods...))
}

// Delete makes a DELETE request and returns a gjson result.
func (client *Client) Delete(path string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodDelete, path, nil, mods...))
}

// DeleteBody makes a DELETE request with a body and returns a gjson result.
// Many Crosswork DELETE operations take a request body, e.g. DLM
// DELETE /crosswork/inventory/v1/credentials and /nodes.
func (client *Client) DeleteBody(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodDelete, path, strings.NewReader(data), mods...))
}
