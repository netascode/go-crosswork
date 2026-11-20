package crosswork

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func status(code int, body string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

// failOnce answers code on the first call to a path and success afterwards.
func failOnce(code int) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(code)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}
}

const credentialsPath = "/crosswork/inventory/v1/credentials"

// retryCase makes a request that fails with 503 once and checks whether it was retried
// (wantCalls 2) or not (wantCalls 1).
func retryCase(t *testing.T, method, path string, wantCalls int, mods ...func(*Req)) {
	t.Helper()
	f := newFake(t)
	f.api = failOnce(http.StatusServiceUnavailable)
	c := f.client(t)
	_, err := c.Do(c.NewReq(method, path, nil, mods...))
	assert.Equal(t, wantCalls, f.callCount(path))
	if wantCalls == 2 {
		assert.NoError(t, err)
	} else {
		assert.Equal(t, 503, StatusCode(err))
	}
}

func TestClientGet_RetriedOn503(t *testing.T) { retryCase(t, http.MethodGet, apiPath, 2) }
func TestClientPut_RetriedOn503(t *testing.T) { retryCase(t, http.MethodPut, credentialsPath, 2) }
func TestClientPost_ReadOnlyQueryRetriedOn503(t *testing.T) {
	retryCase(t, http.MethodPost, "/crosswork/inventory/v1/nodes/query", 2)
}
func TestClientPost_ReadOnlyModRetriedOn503(t *testing.T) {
	retryCase(t, http.MethodPost, "/crosswork/nbi/x/restconf/operations/m:read", 2, ReadOnly)
}
func TestClientPost_RetryNonIdempotentRetriedOn503(t *testing.T) {
	retryCase(t, http.MethodPost, credentialsPath, 2, RetryNonIdempotent)
}
func TestClientPost_CreateNotRetriedOn503(t *testing.T) {
	retryCase(t, http.MethodPost, credentialsPath, 1)
}
func TestClientPatch_NotRetriedOn503(t *testing.T) {
	retryCase(t, http.MethodPatch, "/crosswork/inventory/v1/nodes", 1)
}
func TestClientDelete_NotRetriedOn503(t *testing.T) {
	retryCase(t, http.MethodDelete, credentialsPath, 1)
}

func TestClientDo_NATS500NotRetried(t *testing.T) {
	f := newFake(t)
	f.api = status(500, `{"error":"NATS request failed"}`)
	c := f.client(t, MaxRetries(5))
	_, err := c.Get(apiPath)
	require.Error(t, err)
	assert.Equal(t, 1, f.callCount(apiPath), "a GET that gets the gateway's 500 is not retried")
	assert.Equal(t, 500, StatusCode(err))
	assert.Contains(t, err.Error(), "no such route", "the message says what the 500 means")
}

func TestClientDo_Other500NotRetried(t *testing.T) {
	for name, body := range map[string]string{
		"static resource": `{"code":500,"errorMessage":"No static resource v1/nodes."}`,
		"plain text":      `boom`,
	} {
		f := newFake(t)
		f.api = status(500, body)
		c := f.client(t, MaxRetries(5))
		_, err := c.Get(apiPath)
		require.Error(t, err, name)
		assert.Equal(t, 1, f.callCount(apiPath), name)
		assert.Equal(t, 500, StatusCode(err), name)
	}
}

func TestClientRetryOnErrorCodes(t *testing.T) {
	f := newFake(t)
	f.api = failOnce(http.StatusConflict)
	c := f.client(t, RetryOnErrorCodes([]int{409}))
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	assert.Equal(t, 2, f.callCount(apiPath))
}

func TestClientDo_429WaitsOnce(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}
	c := f.client(t, BackoffMinDelay(1), BackoffMaxDelay(1)) // a 1 s backoff and a 1 s Retry-After
	start := time.Now()
	_, err := c.Post(apiPath, `{}`) // a 429 is safe for every method, even a write
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	assert.Less(t, elapsed, 1900*time.Millisecond, "the larger of backoff and Retry-After, not both in sequence")
	assert.Equal(t, 2, f.callCount(apiPath))
}

func TestClientDo_CancelStopsBackoff(t *testing.T) {
	f := newFake(t)
	f.api = status(503, "")
	c := f.client(t, BackoffMinDelay(30), BackoffMaxDelay(30))
	ctx, cancel := context.WithCancel(context.Background())
	bound := c.WithContext(ctx)
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := bound.Get(apiPath)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 3*time.Second, "a cancelled run must not sit out a 30 s backoff")

	// a per-call Context mod overrides the bound one and also cancels the sleep
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	start = time.Now()
	_, err = c.Get(apiPath, Context(ctx2))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestClientDo_PerRequestTimeout(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) { time.Sleep(2500 * time.Millisecond) }
	c := f.client(t, MaxRetries(0))
	start := time.Now()
	_, err := c.Get(apiPath, Timeout(1))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2200*time.Millisecond)
}

func TestClientDo_RedirectsNotFollowed(t *testing.T) {
	f := newFake(t)
	f.deadStatus = http.StatusFound // the gateway redirects to a login page on another host
	c, err := NewClient(f.URL, "", "", WithToken("not-a-valid-token"))
	require.NoError(t, err)
	_, err = c.Get(apiPath)
	require.Error(t, err)
	assert.Equal(t, 302, StatusCode(err))
	logins, _ := f.counts()
	assert.Equal(t, 0, logins, "a pre-provisioned token never logs in")
}

func TestClientDo_UseMutexSerialisesWrites(t *testing.T) {
	run := func(mods ...func(*Req)) int32 {
		f := newFake(t)
		var inflight, peak int32
		f.api = func(w http.ResponseWriter, r *http.Request, n int) {
			cur := atomic.AddInt32(&inflight, 1)
			for p := atomic.LoadInt32(&peak); cur > p && !atomic.CompareAndSwapInt32(&peak, p, cur); p = atomic.LoadInt32(&peak) {
			}
			time.Sleep(40 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			fmt.Fprint(w, `{"ok":true}`)
		}
		c := f.client(t)
		_, _ = c.Get(apiPath) // log in first
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = c.Put(apiPath, `{}`, mods...) }()
		}
		wg.Wait()
		return atomic.LoadInt32(&peak)
	}
	assert.Equal(t, int32(1), run(UseMutex), "UseMutex makes writes one at a time")
	assert.Greater(t, run(), int32(1), "writes run in parallel by default")
}

func TestIsPreSend(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, dialErr := net.Dial("tcp", addr)
	assert.True(t, isPreSend(dialErr), "connection refused happens before anything is sent")
	assert.False(t, isPreSend(&net.OpError{Op: "read", Err: fmt.Errorf("connection reset by peer")}))
	assert.False(t, isPreSend(context.DeadlineExceeded))
}

func TestRetryAfter(t *testing.T) {
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	assert.Equal(t, time.Duration(0), retryAfter(http.Header{}))
	assert.Equal(t, 7*time.Second, retryAfter(h("7")))
	assert.Equal(t, time.Second, retryAfter(h("0")), "0 waits one second")
	assert.Equal(t, time.Duration(0), retryAfter(h("soon")))
	soon := retryAfter(h(time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)))
	assert.InDelta(t, 10, soon.Seconds(), 2)
}
