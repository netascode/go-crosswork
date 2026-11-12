package crosswork

import (
	"context"
	"net/http"
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
	// returns, so it covers the plan wait (PlanPath) and AfterWrite as well as the request itself.
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
