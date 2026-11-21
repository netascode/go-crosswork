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

const authChangePath = "/crosswork/authconfig/v1/remote/tacacs"

func TestClientDo_AuthenticationChangeIgnoresTheWriteLock(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) { w.WriteHeader(http.StatusNoContent) }
	c := f.client(t)
	c.s.writeMu.Lock()
	defer c.s.writeMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := c.Put(authChangePath, `{}`, UseMutex)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "the write takes its own lock, not the one that is held")
	case <-time.After(3 * time.Second):
		t.Fatal("a write that restarts the authentication service waited for the client's write lock")
	}
}

func TestClientDo_OtherWritesStillTakeTheWriteLock(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) { w.WriteHeader(http.StatusNoContent) }
	c := f.client(t)
	c.s.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := c.Put("/crosswork/aaa/v1/sessionconfig", `{}`, UseMutex)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("the write did not wait for the lock")
	case <-time.After(150 * time.Millisecond):
	}
	c.s.writeMu.Unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the write did not go on after the lock was released")
	}
}

func TestClientDo_AuthenticationChangesAreWrittenOneAtATime(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	var inflight, peak int32
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) {
		cur := atomic.AddInt32(&inflight, 1)
		for p := atomic.LoadInt32(&peak); cur > p && !atomic.CompareAndSwapInt32(&peak, p, cur); p = atomic.LoadInt32(&peak) {
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		w.WriteHeader(http.StatusNoContent)
	}
	c := f.client(t)
	_, _ = c.Get(apiPath) // log in first
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Put(authChangePath, `{}`, UseMutex)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, atomic.LoadInt32(&peak), "the writes to the authentication servers and SSO never overlap")
}

// holdFake returns a fake whose API answers 204 and counts the calls of each path, a client of it, and the function that
// starts a write to the authentication servers and returns the channel of its result.
func holdFake(t *testing.T) (*fakeCW, Client, func() chan error) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, _ int) { w.WriteHeader(http.StatusNoContent) }
	c := f.client(t)
	return f, c, func() chan error {
		done := make(chan error, 1)
		go func() {
			_, err := c.Put(authChangePath, `{}`, UseMutex)
			done <- err
		}()
		return done
	}
}

func TestClientDo_RequestsAreHeldWhileTheAuthenticationServiceRestarts(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.settle = 400 * time.Millisecond
	f, c, write := holdFake(t)
	require.NoError(t, c.Login())
	waiting := write()
	require.Eventually(t, func() bool { return f.callCount(authChangePath) == 1 }, 3*time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond) // the write is noted once the answer has arrived
	sends := map[string]func() error{
		"a write with the client's lock": func() error {
			_, err := c.Put("/crosswork/aaa/v1/passwordPolicyConfig", `{}`, UseMutex)
			return err
		},
		"a read": func() error { _, err := c.Get(apiPath); return err },
	}
	start := time.Now()
	var wg sync.WaitGroup
	for name, send := range sends { // together, so that both are sent while the restart is under way
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, send(), name)
			assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond, name+" is held until the restart is over")
		}()
	}
	wg.Wait()
	require.NoError(t, <-waiting)
}

func TestClientDo_WritesOfTheChangeAreNotHeld(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.settle = 600 * time.Millisecond
	f, c, write := holdFake(t)
	require.NoError(t, c.Login())
	first := write()
	require.Eventually(t, func() bool { return f.callCount(authChangePath) == 1 }, 3*time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	second := write() // a second write of the same burst
	require.Eventually(t, func() bool { return f.callCount(authChangePath) == 2 }, 300*time.Millisecond, 5*time.Millisecond, "a write of the change is sent at once")
	assert.Less(t, time.Since(start), 300*time.Millisecond)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
}

func TestClientDo_NothingIsHeldWithoutARestart(t *testing.T) {
	f, c, _ := holdFake(t)
	require.NoError(t, c.Login())
	start := time.Now()
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	_, err = c.Put("/crosswork/aaa/v1/passwordPolicyConfig", `{}`, UseMutex)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 300*time.Millisecond)
	assert.Equal(t, 0, f.callCount(authChangePath))
}

func TestClientDo_RequestsAreReleasedWhenTheWaitFails(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.timeout = 150 * time.Millisecond
	f, c, write := holdFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 }
	require.NoError(t, c.Login())
	waiting := write()
	require.Eventually(t, func() bool { return f.callCount(authChangePath) == 1 }, 3*time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	_, err := c.Get(apiPath)
	require.NoError(t, err, "a request goes on when the wait has failed")
	var werr *WaitError
	require.ErrorAs(t, <-waiting, &werr)
}

func TestClientDo_HoldEndsWhenNoWaitRuns(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.settle, authChangeWait.timeout, authChangeWait.interval = 50*time.Millisecond, 50*time.Millisecond, time.Millisecond
	f, c, _ := holdFake(t)
	require.NoError(t, c.Login())
	c.s.change.note() // a write whose wait never runs, as when its caller was cancelled
	start := time.Now()
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 90*time.Millisecond)
	assert.Less(t, time.Since(start), 2*time.Second, "the hold cannot block requests for good")
	_ = f
}

func TestClientDo_HeldRequestStopsWithItsContext(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.settle = time.Hour
	_, c, _ := holdFake(t)
	require.NoError(t, c.Login())
	c.s.change.note()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cc := c.WithContext(ctx)
	_, err := cc.Get(apiPath)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
