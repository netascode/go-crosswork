package crosswork

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const apiPath = "/crosswork/inventory/v1/nodes/count"

// age makes the session look as if its token was minted a while ago, so the 30 s guard
// against re-logging-in on a real permission denial does not apply.
func age(c *Client, d time.Duration) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.minted = time.Now().Add(-d)
}

func TestClientLogin(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	logins, mints := f.counts()
	assert.Equal(t, 1, logins)
	assert.Equal(t, 1, mints)
	tok, exp := c.s.current()
	assert.NotEmpty(t, tok)
	assert.Greater(t, time.Until(exp), 59*time.Minute, "the expiry is read from the JWT")
	assert.Equal(t, 0, f.callCount(apiPath), "logging in makes no API call")

	// the Location header of the ticket call names an internal host and is never used
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	logins, _ = f.counts()
	assert.Equal(t, 1, logins, "the token from Login is used, there is no second login")
}

func TestClientAuthenticate_TokenReuse(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	for i := 0; i < 3; i++ {
		res, err := c.Get(apiPath)
		require.NoError(t, err)
		assert.True(t, res.Get("ok").Bool())
	}
	logins, mints := f.counts()
	assert.Equal(t, 1, logins, "one login for three calls")
	assert.Equal(t, 1, mints)
	assert.Contains(t, f.last().Header.Get("Authorization"), "Bearer ")
}

func TestClientLogin_RejectedIsNotRetried(t *testing.T) {
	f := newFake(t)
	f.password = "something-else"
	c := f.client(t, MaxRetries(5))
	_, err := c.Get(apiPath)
	require.Error(t, err)
	assert.Equal(t, 401, StatusCode(err))
	assert.Contains(t, err.Error(), "Invalid credentials")
	assert.Equal(t, 1, f.callCount("/crosswork/sso/v1/tickets"), "a rejected password must never be retried")
	assert.Equal(t, 0, f.callCount(apiPath))
}

func TestClientAuthenticate_ProactiveRefreshUsesTicket(t *testing.T) {
	f := newFake(t)
	f.lifetime = 30 * time.Second // inside the 60 s refresh window
	c := f.client(t)
	for i := 0; i < 3; i++ {
		_, err := c.Get(apiPath)
		require.NoError(t, err)
	}
	logins, mints := f.counts()
	assert.Equal(t, 1, logins, "the password is sent once; later refreshes reuse the ticket")
	assert.Equal(t, 3, mints)
}

func TestClientAuthenticate_DeadTicketFallsBackToLogin(t *testing.T) {
	f := newFake(t)
	f.lifetime = 30 * time.Second
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	f.mu.Lock()
	f.tgts = map[string]bool{} // the ticket is gone but the old JWT still looks valid locally
	f.mu.Unlock()
	_, err = c.Get(apiPath)
	require.NoError(t, err)
	logins, _ := f.counts()
	assert.Equal(t, 2, logins, "a 404 from the JWT call triggers one full login")
}

// reauthOnDeadToken: the session is ended while the JWT still has hours left; Crosswork answers a dead token
// with the given status, and the client logs in once more and retries once.
func reauthOnDeadToken(t *testing.T, status int) {
	t.Helper()
	f := newFake(t)
	f.deadStatus = status
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	f.terminate()
	age(&c, time.Minute)
	res, err := c.Get(apiPath)
	require.NoError(t, err, "one re-login and one retry")
	assert.True(t, res.Get("ok").Bool())
	logins, _ := f.counts()
	assert.Equal(t, 2, logins)
	assert.Equal(t, 3, f.callCount(apiPath), "ok, rejected, retried")
}

func TestClientDo_Reauth403(t *testing.T)                 { reauthOnDeadToken(t, http.StatusForbidden) }
func TestClientDo_Reauth401(t *testing.T)                 { reauthOnDeadToken(t, http.StatusUnauthorized) }
func TestClientDo_ReauthRedirectToLoginPage(t *testing.T) { reauthOnDeadToken(t, http.StatusFound) }

func TestClientDo_Fresh403IsPermissionDenial(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	f.terminate() // pretend the server now refuses us, but the token was minted moments ago
	_, err = c.Get(apiPath)
	require.Error(t, err)
	assert.True(t, IsUnauthorized(err))
	logins, _ := f.counts()
	assert.Equal(t, 1, logins, "no pointless login for a 403 on a token minted under 30 s ago")
}

func TestClientDo_ReauthOnlyOncePerRequest(t *testing.T) {
	f := newFake(t)
	f.deadStatus = http.StatusUnauthorized
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	f.mu.Lock()
	f.rejectAll = true
	f.mu.Unlock()
	_, err = c.Get(apiPath)
	require.Error(t, err)
	assert.Equal(t, 401, StatusCode(err))
	logins, _ := f.counts()
	assert.Equal(t, 2, logins, "exactly one re-login per request, never a loop")
}

func TestClientLogin_ConcurrentCallsLogInOnce(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Get(apiPath)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	logins, _ := f.counts()
	assert.Equal(t, 1, logins)
}

func TestClientDo_ConcurrentReauthLogsInOnce(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	f.terminate()
	age(&c, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Get(apiPath)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	logins, _ := f.counts()
	assert.Equal(t, 2, logins, "single flight: 20 rejected requests share one re-login")
}

func TestClientDo_AccessDeniedDoesNotReauth(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(fixture(t, "restconf_access_denied.json")))
	}
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.Error(t, err)
	age(&c, time.Hour) // an old token: the 30 s guard alone would not stop a re-login
	_, err = c.Get(apiPath)
	require.Error(t, err)
	assert.Equal(t, 403, StatusCode(err))
	assert.Equal(t, "access-denied", asAPIError(err).Code)
	logins, _ := f.counts()
	assert.Equal(t, 1, logins, "a RESTCONF access-denied 403 is a permission denial: no re-login")
}

func TestJwtExpiry(t *testing.T) {
	f := &fakeCW{}
	tok := f.tokenFor(90 * 1e9)
	exp, ok := jwtExpiry(tok)
	assert.True(t, ok)
	assert.WithinDuration(t, exp, exp, 0)
	for _, bad := range []string{"", "a.b", "a.%%%.c", "a.e30.c", "a.bm90LWpzb24.c"} {
		_, ok := jwtExpiry(bad)
		assert.False(t, ok, bad)
	}
}

func TestClientWithToken_NeverLogsIn(t *testing.T) {
	f := newFake(t)
	tok := f.tokenFor(time.Hour)
	f.valid[tok] = true
	c, err := NewClient(f.URL, "", "", WithToken(tok))
	require.NoError(t, err)
	_, err = c.Get(apiPath)
	require.NoError(t, err)
	f.terminate()
	age(&c, time.Hour)
	_, err = c.Get(apiPath)
	assert.True(t, IsUnauthorized(err), "a rejected pre-provisioned token is an error, not a re-login")
	logins, _ := f.counts()
	assert.Equal(t, 0, logins)
	assert.Error(t, c.Login())
}

func TestClientLogout(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	require.Equal(t, 1, f.sessions())

	require.NoError(t, c.Logout())
	assert.Equal(t, 0, f.sessions(), "the ticket is gone, so it no longer counts against the limit")
	del := f.last()
	assert.Equal(t, "DELETE", del.Method)
	assert.Equal(t, "/crosswork/sso/v1/tickets/TGT-1-abc-cas", del.Path)
	assert.True(t, strings.HasPrefix(del.Header.Get("Authorization"), "Bearer "), "the call needs the bearer token")
	tok, _ := c.s.current()
	assert.Empty(t, tok)
	assert.Empty(t, c.s.ticket())
}

func TestClientLogout_NoSession(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Logout())
	assert.Empty(t, f.reqs, "nothing was logged in, so nothing is sent")
}

func TestClientLogout_Twice(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	require.NoError(t, c.Logout())
	n := len(f.reqs)
	require.NoError(t, c.Logout())
	assert.Len(t, f.reqs, n, "the second call sends nothing")
}

func TestClientLogout_WithTokenDoesNothing(t *testing.T) {
	f := newFake(t)
	c := f.client(t, WithToken(f.tokenFor(time.Hour)))
	require.NoError(t, c.Logout())
	assert.Empty(t, f.reqs)
	tok, _ := c.s.current()
	assert.NotEmpty(t, tok, "the supplied token is kept")
}

func TestClientLogout_LogsInAgainAfterwards(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	require.NoError(t, c.Logout())
	_, err = c.Get(apiPath)
	require.NoError(t, err)
	logins, _ := f.counts()
	assert.Equal(t, 2, logins)
	assert.Equal(t, 1, f.sessions())
}

func TestClientLogout_ExpiredJWTGetsANewOneFromTheTicket(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	c.s.mu.Lock()
	c.s.exp = time.Now().Add(-time.Minute) // the JWT is out of date, the ticket is not
	c.s.mu.Unlock()
	require.NoError(t, c.Logout())
	logins, mints := f.counts()
	assert.Equal(t, 1, logins, "no second session is created to log out")
	assert.Equal(t, 2, mints)
	assert.Equal(t, 0, f.sessions())
}

func TestClientLogout_AlreadyEnded(t *testing.T) {
	for name, status := range map[string]int{"403": 403, "401": 401, "404": 404} {
		f := newFake(t)
		c := f.client(t)
		require.NoError(t, c.Login())
		f.logoutCode = status
		assert.NoError(t, c.Logout(), name)
		assert.Empty(t, c.s.ticket(), name)
	}
}

func TestClientLogout_ServerErrorKeepsTheSession(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	f.logoutCode = 500
	err := c.Logout()
	require.Error(t, err)
	assert.Equal(t, 500, StatusCode(err))
	assert.NotEmpty(t, c.s.ticket(), "the ticket is kept so that Logout can be tried again")
	f.logoutCode = 0
	require.NoError(t, c.Logout())
	assert.Equal(t, 0, f.sessions())
}

func TestClientLogout_SharedByCopies(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	cp := c.WithContext(context.Background())
	require.NoError(t, cp.Login())
	require.NoError(t, c.Logout())
	assert.Equal(t, 0, f.sessions(), "a copy and the original share one session")
}

func TestClientWaitForLogin_RecoversAfterFailures(t *testing.T) {
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 && n <= 4 } // call 1 is the login itself
	c := f.client(t)
	require.NoError(t, c.Login())
	require.NoError(t, c.WaitForLogin(0, time.Millisecond, 5*time.Second))
	assert.Equal(t, 6, f.callCount("/crosswork/sso/v2/tickets/jwt"), "three failures, then two successes in a row")
	logins, _ := f.counts()
	assert.Equal(t, 1, logins, "the ticket of the session is reused, no new session is opened")
}

func TestClientWaitForLogin_OneSuccessIsNotEnough(t *testing.T) {
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n == 3 } // login, ok, fail, ok, ok
	c := f.client(t)
	require.NoError(t, c.Login())
	require.NoError(t, c.WaitForLogin(0, time.Millisecond, 5*time.Second))
	assert.Equal(t, 5, f.callCount("/crosswork/sso/v2/tickets/jwt"))
}

func TestClientWaitForLogin_NeverRecovers(t *testing.T) {
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 }
	c := f.client(t)
	require.NoError(t, c.Login())
	err := c.WaitForLogin(0, time.Millisecond, 100*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "had not accepted a new login")
	assert.Equal(t, http.StatusInternalServerError, StatusCode(err), "the last status of Crosswork is kept")
}

func TestClientWaitForLogin_SettlesBeforeTheFirstCall(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	start := time.Now()
	require.NoError(t, c.WaitForLogin(150*time.Millisecond, time.Millisecond, 5*time.Second))
	assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)
}

func TestClientWaitForLogin_DeadTicketLogsInAgain(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	f.terminate()
	require.NoError(t, c.WaitForLogin(0, time.Millisecond, 5*time.Second))
	logins, _ := f.counts()
	assert.Equal(t, 2, logins)
}

func TestClientWaitForLogin_ContextCancelled(t *testing.T) {
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 }
	c := f.client(t)
	require.NoError(t, c.Login())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cc := c.WithContext(ctx)
	err := cc.WaitForLogin(0, time.Millisecond, 10*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClientWaitForLogin_WithTokenDoesNothing(t *testing.T) {
	f := newFake(t)
	c := f.client(t, WithToken(f.tokenFor(time.Hour)))
	require.NoError(t, c.WaitForLogin(time.Hour, time.Hour, time.Hour))
	assert.Equal(t, 0, f.callCount("/crosswork/sso/v2/tickets/jwt"))
}

func shortAuthChangeWait(t *testing.T) {
	old := authChangeWait
	authChangeWait = loginWait{settle: 0, interval: time.Millisecond, timeout: 5 * time.Second}
	t.Cleanup(func() { authChangeWait = old })
}

func TestClientPut_AuthenticationChangeWaitsForLogin(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusNoContent) }
	// call 1 is the login; the window opens after the write (call 2 is the first probe) and closes after three failures
	f.jwtFail = func(n int) bool { return n >= 2 && n <= 4 }
	c := f.client(t)
	_, err := c.Put("/crosswork/authconfig/v1/remote/tacacs", `{"tacacs_servers":[]}`, UseMutex)
	require.NoError(t, err)
	assert.Equal(t, 6, f.callCount("/crosswork/sso/v2/tickets/jwt"), "three failures, then two successes")
}

func TestClientPut_AuthenticationChangeThatNeverRecoversFails(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.timeout = 100 * time.Millisecond
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusNoContent) }
	f.jwtFail = func(n int) bool { return n >= 2 }
	c := f.client(t)
	_, err := c.Put("/crosswork/authconfig/v1/sso/idp/services", `{"registeredServiceProvider":[]}`, UseMutex)
	var werr *WaitError
	require.ErrorAs(t, err, &werr)
	assert.Contains(t, err.Error(), "had not accepted a new login")
}

func TestClientPut_OtherAuthenticationWritesDoNotWait(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusNoContent) }
	c := f.client(t)
	_, err := c.Put("/crosswork/aaa/v1/sessionconfig", `{}`, UseMutex)
	require.NoError(t, err)
	assert.Equal(t, 1, f.callCount("/crosswork/sso/v2/tickets/jwt"), "only the login itself asked for a token")
}

func TestClientPut_AuthenticationChangeKeepsAnExplicitAfterWrite(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusNoContent) }
	called := false
	c := f.client(t)
	_, err := c.Put("/crosswork/authconfig/v1/remote/tacacs", `{}`, AfterWrite(func(_ context.Context, _ Res) error { called = true; return nil }))
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, 1, f.callCount("/crosswork/sso/v2/tickets/jwt"), "the caller's function replaces the default wait")
}

func shortLoginWindow(t *testing.T) {
	old := loginWindow
	loginWindow = loginTiming{retryInterval: time.Millisecond, retryBudget: 5 * time.Second, callTimeout: 5 * time.Second}
	t.Cleanup(func() { loginWindow = old })
}

func TestIsAuthRestart(t *testing.T) {
	restart := `{"error":"There was a problem proxying the request"}`
	for name, tc := range map[string]struct {
		status int
		body   string
		want   bool
	}{
		"the restart":              {500, restart, true},
		"another 500":              {500, `{"error":"boom"}`, false},
		"the message with a 502":   {502, restart, false},
		"a rejected password":      {401, `{"authentication_exceptions":["Invalid credentials"]}`, false},
		"no answer at all":         {0, "", false},
		"a 500 with an empty body": {500, "", false},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, tc.want, isAuthRestart(tc.status, tc.body)) })
	}
}

func TestIsHeld(t *testing.T) {
	assert.False(t, isHeld(nil))
	assert.False(t, isHeld(http.ErrServerClosed), "an error that is not a timeout")
	assert.True(t, isHeld(&url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}), "no answer in time")
	assert.False(t, isHeld(&net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}), "no connection was made")
}

func TestClientLogin_WaitsOutTheRestart(t *testing.T) {
	shortLoginWindow(t)
	f := newFake(t)
	f.ticketFail = func(n int) bool { return n <= 2 }
	f.jwtFail = func(n int) bool { return n <= 2 }
	c := f.client(t)
	require.NoError(t, c.Login())
	assert.Equal(t, 3, f.callCount("/crosswork/sso/v1/tickets"), "two refused, then one that works")
	assert.Equal(t, 3, f.callCount("/crosswork/sso/v2/tickets/jwt"))
	assert.Equal(t, 1, f.sessions())
}

func TestClientLogin_OtherServerErrorsFailAtOnce(t *testing.T) {
	shortLoginWindow(t)
	for name, tc := range map[string]struct {
		mod               func(f *fakeCW)
		tickets, jwtCalls int
	}{
		"ticket": {func(f *fakeCW) { f.ticketFail = func(int) bool { return true } }, 1, 0},
		"jwt":    {func(f *fakeCW) { f.jwtFail = func(int) bool { return true } }, 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.plain500 = true // a 500 that is not the answer of the restart
			tc.mod(f)
			c := f.client(t)
			err := c.Login()
			require.Error(t, err)
			assert.Equal(t, http.StatusInternalServerError, StatusCode(err))
			assert.Equal(t, tc.tickets, f.callCount("/crosswork/sso/v1/tickets"), "one attempt")
			assert.Equal(t, tc.jwtCalls, f.callCount("/crosswork/sso/v2/tickets/jwt"), "one attempt")
		})
	}
}

func TestClientLogin_RejectedPasswordFailsAtOnce(t *testing.T) {
	shortLoginWindow(t)
	f := newFake(t)
	f.password = "other"
	c := f.client(t)
	err := c.Login()
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, StatusCode(err))
	assert.Equal(t, 1, f.callCount("/crosswork/sso/v1/tickets"))
}

func TestClientLogin_GivesUpWhenTheRestartDoesNotEnd(t *testing.T) {
	shortLoginWindow(t)
	loginWindow.retryBudget = 50 * time.Millisecond
	f := newFake(t)
	f.jwtFail = func(int) bool { return true }
	c := f.client(t)
	err := c.Login()
	require.Error(t, err)
	assert.Equal(t, http.StatusInternalServerError, StatusCode(err), "the answer of Crosswork is returned")
	assert.Greater(t, f.callCount("/crosswork/sso/v2/tickets/jwt"), 1, "it tried more than once")
}

func TestClientLogin_HeldRequestGetsTheLongTimeout(t *testing.T) {
	shortLoginWindow(t)
	f := newFake(t)
	f.jwtHold = func(n int) time.Duration {
		if n == 1 {
			return 400 * time.Millisecond
		}
		return 0
	}
	c := f.client(t)
	c.HttpClient.Timeout = 100 * time.Millisecond
	require.NoError(t, c.Login())
	assert.Equal(t, 2, f.callCount("/crosswork/sso/v2/tickets/jwt"), "the held request timed out and was tried again")
}

func TestClientWaitForLogin_PollIsNotCutOffByTheClientTimeout(t *testing.T) {
	shortLoginWindow(t)
	f := newFake(t)
	f.jwtHold = func(n int) time.Duration {
		if n == 2 { // call 1 is the login
			return 300 * time.Millisecond
		}
		return 0
	}
	c := f.client(t)
	require.NoError(t, c.Login())
	c.HttpClient.Timeout = 100 * time.Millisecond
	require.NoError(t, c.WaitForLogin(0, time.Millisecond, 5*time.Second))
	assert.Equal(t, 3, f.callCount("/crosswork/sso/v2/tickets/jwt"), "the login, the held poll (the first confirmation) and the second confirmation")
}

func TestAwaitAuthChange_BurstSharesOneWait(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 && n <= 4 }
	c := f.client(t)
	require.NoError(t, c.Login())
	writes := make([]uint64, 4)
	for i := range writes {
		writes[i] = c.s.change.note()
	}
	errs := make([]error, len(writes))
	var wg sync.WaitGroup
	for i := range writes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.awaitAuthChange(writes[i])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 6, f.callCount("/crosswork/sso/v2/tickets/jwt"), "the login, three failures and two successes: one wait for four writes")
}

func TestAwaitAuthChange_LaterWriteGetsItsOwnWait(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	require.NoError(t, c.awaitAuthChange(c.s.change.note()))
	assert.Equal(t, 3, f.callCount("/crosswork/sso/v2/tickets/jwt"), "the login and two successes")
	require.NoError(t, c.awaitAuthChange(c.s.change.note()))
	assert.Equal(t, 5, f.callCount("/crosswork/sso/v2/tickets/jwt"), "a write made after the wait is not covered by it")
}

func TestAwaitAuthChange_FailureIsSharedNotRepeated(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.timeout = 200 * time.Millisecond
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 }
	c := f.client(t)
	require.NoError(t, c.Login())
	writes := []uint64{c.s.change.note(), c.s.change.note(), c.s.change.note()}
	errs := make([]error, len(writes))
	var wg sync.WaitGroup
	start := time.Now()
	for i := range writes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.awaitAuthChange(writes[i])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.Error(t, err)
		assert.Contains(t, err.Error(), "had not accepted a new login")
	}
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the three writers share one failed wait, not three")
}

func TestAwaitAuthChange_PauseCountsFromTheLatestWrite(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.settle = 200 * time.Millisecond
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	start := time.Now()
	n := c.s.change.note()
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, c.awaitAuthChange(n))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond, "the pause is over before the first poll")
	assert.Less(t, elapsed, 345*time.Millisecond, "the time since the write counts towards the pause")
}

func TestAwaitAuthChange_CancelledWaitIsNotRecorded(t *testing.T) {
	shortAuthChangeWait(t)
	f := newFake(t)
	f.jwtFail = func(n int) bool { return n >= 2 && n <= 1000 }
	c := f.client(t)
	require.NoError(t, c.Login())
	n := c.s.change.note()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cc := c.WithContext(ctx)
	require.Error(t, cc.awaitAuthChange(n))
	done, _ := c.s.change.outcome(n)
	assert.False(t, done, "a wait that was cancelled says nothing about the restart")
}

func TestClientLogin_HeldRequestsStopAtTheBudget(t *testing.T) {
	shortLoginWindow(t)
	loginWindow.retryBudget = 150 * time.Millisecond
	loginWindow.callTimeout = 60 * time.Millisecond
	f := newFake(t)
	f.jwtHold = func(int) time.Duration { return 300 * time.Millisecond }
	c := f.client(t)
	c.HttpClient.Timeout = 40 * time.Millisecond
	start := time.Now()
	err := c.Login()
	require.Error(t, err)
	assert.True(t, isHeld(err), "the request was held: %v", err)
	assert.Less(t, time.Since(start), time.Second, "no retries on top of the budget")
}

func TestAwaitAuthChange_WritesDuringThePauseShareThePoll(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.settle = 300 * time.Millisecond
	f := newFake(t)
	c := f.client(t)
	require.NoError(t, c.Login())
	var wg sync.WaitGroup
	errs := make([]error, 3)
	start := time.Now()
	for i := range errs {
		n := c.s.change.note()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.awaitAuthChange(n)
		}()
		time.Sleep(100 * time.Millisecond) // the next write lands while the first waiter is still pausing
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 3, f.callCount("/crosswork/sso/v2/tickets/jwt"), "the login and one poll of two successes for the three writes")
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "the pause ran from the latest write (at 200 ms)")
}

func TestAwaitAuthChange_WriteDuringThePollStartsTheWaitAgain(t *testing.T) {
	shortAuthChangeWait(t)
	authChangeWait.timeout = 150 * time.Millisecond
	f := newFake(t)
	var failing atomic.Bool
	failing.Store(true)
	f.jwtFail = func(n int) bool { return n >= 2 && failing.Load() }
	c := f.client(t)
	require.NoError(t, c.Login())
	first := c.s.change.note()
	done := make(chan error, 1)
	go func() { done <- c.awaitAuthChange(first) }()
	time.Sleep(50 * time.Millisecond)
	second := c.s.change.note() // lands while the poll of the first write runs
	select {
	case err := <-done:
		t.Fatalf("the wait ended with %v although a later write had started another restart", err)
	case <-time.After(200 * time.Millisecond): // the first poll ran out its limit at 150 ms; the wait started again
	}
	failing.Store(false)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the wait did not end after logins worked again")
	}
	require.NoError(t, c.awaitAuthChange(second))
}
