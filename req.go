package crosswork

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Body wraps SJSON for building JSON body strings.
// Usage example:
//
//	Body{}.Set("name", "ABC").Str
type Body struct {
	Str string
}

// Set sets a JSON path to a value. Any JSON-encodable value is accepted, so numbers
// and booleans keep their type.
func (body Body) Set(path string, value interface{}) Body {
	res, _ := sjson.Set(body.Str, path, value)
	body.Str = res
	return body
}

// SetRaw sets a JSON path to a raw string value.
// This is primarily used for building up nested structures, e.g.:
//
//	Body{}.SetRaw("children", Body{}.Set("name", "New").Str).Str
func (body Body) SetRaw(path, rawValue string) Body {
	res, _ := sjson.SetRaw(body.Str, path, rawValue)
	body.Str = res
	return body
}

// Delete deletes a JSON path.
func (body Body) Delete(path string) Body {
	res, _ := sjson.Delete(body.Str, path)
	body.Str = res
	return body
}

// Res creates a Res object, i.e. a GJSON result object.
func (body Body) Res() Res {
	return gjson.Parse(body.Str)
}

// Req wraps http.Request for API requests.
type Req struct {
	// HttpReq is the *http.Request object.
	HttpReq *http.Request
	// LogPayload indicates whether logging of payloads should be enabled.
	LogPayload bool
	// UseMutex indicates whether to use the client's writing mutex for this request. The mutex is held until Do
	// returns, so it covers the plan wait (PlanPath) and AfterWrite as well as the request itself. A PUT to the
	// remote authentication servers or the SSO service providers takes a lock of its own instead (see restartsLogin).
	UseMutex bool
	// MutexKey names a lock of its own for this request (see UseMutexKey). Requests with the same key run one at a
	// time, requests with different keys, and requests with UseMutex, do not wait for each other.
	MutexKey string
	// AfterWrite is called after a successful PUT, POST, PATCH or DELETE, and after the plan wait, while the lock is
	// still held (see AfterWrite). It receives the context of the request and the response of the write.
	AfterWrite func(ctx context.Context, res Res) error
	// ReadOnly marks a POST (or RPC) as a read, so it is retried like a GET.
	ReadOnly bool
	// RetryNonIdempotent allows retries after an ambiguous failure for any method.
	RetryNonIdempotent bool
	// Timeout is a per-request timeout; zero uses the client's timeout (120 seconds for writes
	// under the NSO proxy). It may be longer than the client's timeout.
	Timeout time.Duration
	// MaxAsyncWaitTime is the longest time in seconds to wait for the NSO plan of a write (see
	// PlanPath); zero uses Client.DefaultMaxAsyncWaitTime.
	MaxAsyncWaitTime int
	// NoWait skips the wait for the plan even when PlanPath is set.
	NoWait bool
	// PlanPath is the full path of the NSO plan of the object this write changes. When set, a
	// successful PUT, POST or PATCH waits until the plan is ready and a successful DELETE waits
	// until the plan is gone. NSO does not return a handle on a write, so the caller supplies it.
	PlanPath string

	resultCheck     func(Res) error
	noResultCheck   bool
	reAuthAttempted bool
	noNSORetry      bool
	buildErr        error
}

// NoLogPayload prevents logging of payloads.
// Used for login and for calls that carry secrets such as credential profiles.
func NoLogPayload(req *Req) {
	req.LogPayload = false
}

// UseMutex enables the use of the writing mutex for this request.
func UseMutex(req *Req) {
	req.UseMutex = true
}

// UseMutexKey makes this request take the lock with the given name, a lock that only requests with the same key
// share, held until Do returns (the response, the plan wait and AfterWrite included). A GET never takes it. The wait for
// the lock ends with the error of the context when the context of the request is cancelled. Unlike UseMutex it
// does not take the client's writing mutex as well. Use one key for each group of writes that must not overlap.
func UseMutexKey(key string) func(*Req) {
	return func(req *Req) {
		req.MutexKey = key
	}
}

// AfterWrite sets a function that Do calls after a successful write (and after the plan wait), still inside the lock of
// UseMutex or UseMutexKey, so a caller can hold the lock until the work that the write started is finished. The
// function gets the context of the request and the response of the write. When it fails, Do returns the response of the
// write together with a *WaitError. It is not called for a GET or for a write that failed.
//
// The function and the requests it makes must not ask for the same key: a lock is not reentrant. Its GETs never
// take a lock.
func AfterWrite(fn func(ctx context.Context, res Res) error) func(*Req) {
	return func(req *Req) {
		req.AfterWrite = fn
	}
}

// ReadOnly marks a POST or RPC as a read-only call, so it may be retried after an ambiguous failure.
func ReadOnly(req *Req) {
	req.ReadOnly = true
}

// RetryNonIdempotent allows this request to be retried after an ambiguous failure
// (connection reset after sending, timeout, 502/503/504) even if it is a write.
func RetryNonIdempotent(req *Req) {
	req.RetryNonIdempotent = true
}

// NoResultCheck disables the detection of failures inside HTTP 200 responses for this request.
func NoResultCheck(req *Req) {
	req.noResultCheck = true
}

// ResultCheck replaces the built-in detection of failures inside HTTP 200 responses for this request.
// The function is called with every 2xx response; a non-nil error is returned to the caller.
func ResultCheck(fn func(Res) error) func(*Req) {
	return func(req *Req) {
		req.resultCheck = fn
	}
}

// Query sets an HTTP query parameter.
//
//	client.Get(path, crosswork.Query("content", "config"), crosswork.Query("depth", "1"))
func Query(k, v string) func(req *Req) {
	return func(req *Req) {
		q := req.HttpReq.URL.Query()
		q.Add(k, v)
		req.HttpReq.URL.RawQuery = q.Encode()
	}
}

// Timeout sets a custom timeout for this specific request in seconds.
func Timeout(timeoutSeconds int) func(req *Req) {
	return func(req *Req) {
		req.Timeout = time.Duration(timeoutSeconds) * time.Second
	}
}

// Context binds a context to this request, overriding the one set with Client.WithContext.
func Context(ctx context.Context) func(req *Req) {
	return func(req *Req) {
		req.HttpReq = req.HttpReq.WithContext(ctx)
	}
}

// ContentType sets the Content-Type header, overriding the default selected from the path.
func ContentType(v string) func(req *Req) {
	return func(req *Req) {
		req.HttpReq.Header.Set("Content-Type", v)
	}
}

// Accept sets the Accept header, overriding the default selected from the path.
func Accept(v string) func(req *Req) {
	return func(req *Req) {
		req.HttpReq.Header.Set("Accept", v)
	}
}

// MaxAsyncWaitTime sets the longest time in seconds to wait for the NSO plan of this write.
// The default is Client.DefaultMaxAsyncWaitTime.
func MaxAsyncWaitTime(seconds int) func(req *Req) {
	return func(req *Req) {
		req.MaxAsyncWaitTime = seconds
	}
}

// NoWait makes a write return as soon as NSO accepted it, without waiting for the plan.
func NoWait(req *Req) {
	req.NoWait = true
}

// PlanPath sets the full path of the NSO plan of the object this write changes, so the write
// waits for the plan (see Req.PlanPath and WaitForPlan).
//
//	client.Delete(path, crosswork.PlanPath(planPath))
func PlanPath(path string) func(req *Req) {
	return func(req *Req) {
		req.PlanPath = path
	}
}

// Crosswork-specific behaviour that depends on the request path lives here. Callers pass full paths,
// so there are no base-path constants: the library only recognises a few path shapes.

const (
	jsonType = "application/json"
	// jsonBodyTyp is sent as Content-Type for JSON bodies: the aaa gateway answers 405 to a bare application/json.
	jsonBodyTyp = "application/json; charset=UTF-8"
	restconfTyp = "application/yang-data+json"
	patchType   = "application/yang-patch+json"
)

// matchPath returns the request path normalised for matching: runs of slashes are
// collapsed (some documented paths contain //) and anything before /crosswork/ is
// dropped, so a configured URL path prefix does not matter.
func matchPath(u *url.URL) string {
	p := collapseSlashes(u.Path)
	if i := strings.Index(p, "/crosswork/"); i > 0 {
		p = p[i:]
	}
	return p
}

// isRestconf reports whether a path belongs to a RESTCONF API (they contain /restconf/).
func isRestconf(p string) bool {
	return strings.Contains(p, "/restconf/") || strings.HasSuffix(p, "/restconf")
}

// readOnlyPost reports whether a POST path is a read: Crosswork queries are POSTs.
func readOnlyPost(p string) bool {
	p = strings.TrimSuffix(p, "/")
	for _, s := range []string{"/query", "/querybrief", "/count"} {
		if strings.HasSuffix(p, s) {
			return true
		}
	}
	return false
}

// restartsLogin reports whether a write to the path changes the remote authentication servers or the SSO
// service providers. Crosswork restarts its authentication service for such a change, and for a while
// every new login fails or is held. A PUT to such a path is handled differently from other writes:
//   - It takes a lock of its own, not the client's write lock that UseMutex asks for, so it holds up no other
//     write. The writes to these paths still run one at a time.
//   - Do lets go of that lock as soon as the write is done and then waits for logins to work again before it
//     returns, unless the request has its own AfterWrite function. The writers of one burst share that wait.
//   - Every login call waits out the restart (see postForm).
func restartsLogin(p string) bool {
	for _, prefix := range []string{
		"/crosswork/authconfig/v1/remote/",
		"/crosswork/authconfig/v2/remote/",
		"/crosswork/authconfig/v1/sso/idp/services",
	} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// dgManagerPrefix is the path of the Data Gateway manager, which answers 400 ("received
// unexpected Content-Type") to the charset form of the JSON content type.
const dgManagerPrefix = "/crosswork/dg-manager/"

// applyDefaultHeaders sets Content-Type and Accept from the path unless a request mod
// (or YangPatch) already set them: yang-data+json for RESTCONF paths, a bare application/json
// for the Data Gateway manager, JSON with a charset otherwise.
func applyDefaultHeaders(req *Req) {
	t, ct := jsonType, jsonBodyTyp
	p := matchPath(req.HttpReq.URL)
	switch {
	case isRestconf(p):
		t, ct = restconfTyp, restconfTyp
	case strings.HasPrefix(p, dgManagerPrefix):
		ct = jsonType
	}
	if req.HttpReq.Header.Get("Content-Type") == "" {
		req.HttpReq.Header.Set("Content-Type", ct)
	}
	if req.HttpReq.Header.Get("Accept") == "" {
		req.HttpReq.Header.Set("Accept", t)
	}
}

// canRetryAmbiguous reports whether a request may be retried after a failure whose
// outcome is unknown (connection reset after sending, timeout, 502/503/504). Reads,
// read-only POSTs and PUT are repeatable. POST creates, PATCH and DELETE are not: a
// repeated DLM create fails ("already exists") and a repeated DLM delete fails, which
// would turn a success into a failure.
func canRetryAmbiguous(req Req) bool {
	if req.RetryNonIdempotent {
		return true
	}
	switch req.HttpReq.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut:
		return true
	case http.MethodPost:
		return req.ReadOnly || readOnlyPost(matchPath(req.HttpReq.URL))
	}
	return false
}

// isAuthFailure reports whether a response means the token is no longer accepted.
// Crosswork answers a dead token with 403 (not 401), and some paths with a redirect to
// the SSO login page. The response body is never inspected.
func isAuthFailure(status int, h http.Header) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		u, err := url.Parse(h.Get("Location"))
		return err == nil && strings.HasPrefix(collapseSlashes(u.Path), "/crosswork/sso/login")
	}
	return false
}

// nsoWriteTimeout is the request timeout of a write under the NSO proxy when the caller sets
// none: the call includes the device push, which can outlast the 60 second client default.
const nsoWriteTimeout = 120 * time.Second

// defaultTimeout returns the timeout for a request that has no Timeout of its own, or zero to
// use the client's timeout.
func defaultTimeout(req Req) time.Duration {
	if req.HttpReq.Method != http.MethodGet && req.HttpReq.Method != http.MethodHead &&
		strings.HasPrefix(matchPath(req.HttpReq.URL), nsoProxyPrefix) {
		return nsoWriteTimeout
	}
	return 0
}
