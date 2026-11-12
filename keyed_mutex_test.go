package crosswork

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// peakOfWrites sends writes from goroutines, each with the options that mods(i) returns, and returns the most
// requests that the server had in progress at once.
func peakOfWrites(t *testing.T, n int, mods func(i int) []func(*Req)) int32 {
	f := newFake(t)
	var inflight, peak int32
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) {
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
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _ = c.Put(apiPath, `{}`, mods(i)...) }(i)
	}
	wg.Wait()
	return atomic.LoadInt32(&peak)
}

// Writes with the same key run one at a time; different keys, no key and UseMutex do not wait for each other.
func TestClientDo_UseMutexKey(t *testing.T) {
	assert.Equal(t, int32(1), peakOfWrites(t, 8, func(int) []func(*Req) { return []func(*Req){UseMutexKey("a")} }), "one key, one at a time")
	assert.Greater(t, peakOfWrites(t, 8, func(i int) []func(*Req) {
		return []func(*Req){UseMutexKey(fmt.Sprintf("k%d", i%2))}
	}), int32(1), "two keys run side by side")
	assert.Greater(t, peakOfWrites(t, 8, func(i int) []func(*Req) {
		if i%2 == 0 {
			return []func(*Req){UseMutex}
		}
		return []func(*Req){UseMutexKey("a")}
	}), int32(1), "the default lock and a key are different locks")
	assert.Equal(t, int32(1), peakOfWrites(t, 8, func(int) []func(*Req) { return []func(*Req){UseMutex} }), "UseMutex behaves as before")
}

// A GET never takes a keyed lock.
func TestClientDo_UseMutexKeyIgnoresGET(t *testing.T) {
	f := newFake(t)
	var inflight, peak int32
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) {
		cur := atomic.AddInt32(&inflight, 1)
		for p := atomic.LoadInt32(&peak); cur > p && !atomic.CompareAndSwapInt32(&peak, p, cur); p = atomic.LoadInt32(&peak) {
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		fmt.Fprint(w, `{"ok":true}`)
	}
	c := f.client(t)
	_, _ = c.Get(apiPath)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Get(apiPath, UseMutexKey("a")) }()
	}
	wg.Wait()
	assert.Greater(t, atomic.LoadInt32(&peak), int32(1))
}

// A request that waits for a key gives up when its context is cancelled; the holder is not affected.
func TestClientDo_UseMutexKeyContext(t *testing.T) {
	f := newFake(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == http.MethodPut {
			once.Do(func() { close(started) })
			<-release
		}
		fmt.Fprint(w, `{"ok":true}`)
	}
	c := f.client(t)
	_, _ = c.Get(apiPath)
	holder := make(chan error, 1)
	go func() { _, err := c.Put(apiPath, `{}`, UseMutexKey("a")); holder <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := c.Put(apiPath, `{}`, UseMutexKey("a"), Context(ctx))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(t0), 2*time.Second)
	close(release)
	assert.NoError(t, <-holder)
	_, err = c.Put(apiPath, `{}`, UseMutexKey("a"))
	assert.NoError(t, err, "the lock is free again")
}

// AfterWrite runs after a successful write, inside the lock: a second write with the key starts only when it returned.
func TestClientDo_AfterWriteHoldsTheLock(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var events []string
	log := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == http.MethodPut {
			log("write " + r.URL.Query().Get("n"))
		}
		fmt.Fprint(w, `{"key":"id-`+r.URL.Query().Get("n")+`"}`)
	}
	c := f.client(t)
	_, _ = c.Get(apiPath)
	var wg sync.WaitGroup
	for _, n := range []string{"1", "2"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_, err := c.Put(apiPath+"?n="+n, `{}`, UseMutexKey("a"), AfterWrite(func(ctx context.Context, res Res) error {
				log("wait start " + res.Get("key").String())
				time.Sleep(60 * time.Millisecond)
				log("wait end " + res.Get("key").String())
				return nil
			}))
			assert.NoError(t, err)
		}(n)
	}
	wg.Wait()
	require.Len(t, events, 6)
	// whichever write came first, its wait ended before the other write was sent
	first := strings.TrimPrefix(events[0], "write ")
	assert.Equal(t, []string{"write " + first, "wait start id-" + first, "wait end id-" + first}, events[:3])
}

// AfterWrite is not called for a GET or a failed write; its error comes back as a WaitError with the response of the write.
func TestClientDo_AfterWriteErrors(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"no"}`)
			return
		}
		fmt.Fprint(w, `{"key":"job-9"}`)
	}
	c := f.client(t)
	var calls int32
	count := func(ctx context.Context, res Res) error { atomic.AddInt32(&calls, 1); return nil }

	_, err := c.Get(apiPath, AfterWrite(count))
	require.NoError(t, err)
	_, err = c.Put(apiPath+"?fail=1", `{}`, AfterWrite(count))
	require.Error(t, err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls), "not called for a GET or a failed write")

	boom := errors.New("the job did not finish")
	res, err := c.Post(apiPath, `{}`, AfterWrite(func(context.Context, Res) error { return boom }))
	var we *WaitError
	require.ErrorAs(t, err, &we)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, "job-9", res.Get("key").String(), "the response of the write is returned with the wait error")
	var ae *APIError
	assert.False(t, errors.As(err, &ae), "a wait error is not an API error")
}

// WaitForMopJob reads the job id from `key` and holds the lock until the job has finished.
func TestWaitForMopJob(t *testing.T) {
	f := newFake(t)
	mop, polls := mopServer(t, []string{"Pending", "Running", "Success"}, "{}", nil)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"key":"70"}`)
			return
		}
		mop(w, r, n)
	}
	c := f.client(t)
	res, err := c.Post(apiPath, `{}`, UseMutexKey("gc"), c.WaitForMopJob(time.Millisecond, 5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, "70", res.Get("key").String())
	assert.Equal(t, int32(3), atomic.LoadInt32(polls), "polled until the job was finished")

	// a job that is still running at the timeout
	f = newFake(t)
	mop, _ = mopServer(t, []string{"Running"}, "{}", nil)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"key":"70"}`)
			return
		}
		mop(w, r, n)
	}
	c = f.client(t)
	res, err = c.Post(apiPath, `{}`, c.WaitForMopJob(5*time.Millisecond, 40*time.Millisecond))
	var we *WaitError
	require.ErrorAs(t, err, &we)
	assert.ErrorIs(t, err, ErrMopJobWait)
	assert.Equal(t, "70", res.Get("key").String())

	// an answer without a job id
	f = newFake(t)
	f.api = status(200, `{"ok":true}`)
	c = f.client(t)
	_, err = c.Post(apiPath, `{}`, c.WaitForMopJob(time.Millisecond, time.Second))
	require.ErrorAs(t, err, &we)
	assert.Contains(t, err.Error(), "no job id")
}
