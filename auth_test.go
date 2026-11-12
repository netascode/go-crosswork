package crosswork

import (
	"context"
	"net/http"
	"strings"
	"sync"
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
