package crosswork

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Live tests talk to a real Crosswork and are skipped unless CW_URL, CW_USERNAME and
// CW_PASSWORD are set. Credentials come from the environment only. The write test also
// needs CW_WRITE=1 and creates and removes one credential profile named cwtest-live.
// newLiveClient makes a client for the cluster named by the environment, or skips the test.
func newLiveClient(t *testing.T) Client {
	t.Helper()
	url, usr, pwd := os.Getenv("CW_URL"), os.Getenv("CW_USERNAME"), os.Getenv("CW_PASSWORD")
	if url == "" || usr == "" || pwd == "" {
		t.Skip("set CW_URL, CW_USERNAME and CW_PASSWORD to run the live tests")
	}
	c, err := NewClient(url, usr, pwd, UserAgent("go-crosswork live test"))
	require.NoError(t, err)
	return c
}

// liveClient is newLiveClient plus a Logout when the test ends. Crosswork limits the sessions
// per user, and a test that never logs out keeps its session until it times out.
func liveClient(t *testing.T) Client {
	t.Helper()
	c := newLiveClient(t)
	t.Cleanup(func() {
		if err := c.Logout(); err != nil {
			t.Logf("logout failed: %v", err)
		}
	})
	return c
}

func TestLiveClientGet_UserInfo(t *testing.T) {
	c := liveClient(t)
	res, err := c.Get("/crosswork/sso/v2/userInfo")
	require.NoError(t, err)
	assert.NotEmpty(t, res.Get("user").String())
	t.Logf("logged in as %s (role %s)", res.Get("user").String(), res.Get("role").String())
}

func TestLiveClientPost_CredentialProfileCycle(t *testing.T) {
	c := liveClient(t)
	if os.Getenv("CW_WRITE") != "1" {
		t.Skip("set CW_WRITE=1 to run the live test that creates and deletes a credential profile")
	}
	const (
		base    = "/crosswork/inventory/v1/credentials"
		profile = "cwtest-live"
	)
	create := Body{}.SetRaw("data", `[{"profile":"`+profile+`","user_pass":[{"user_name":"cw","password":"Cw-Live-1!","type":"ROBOT_USERPASS_SSH"}]}]`).Str
	del := Body{}.SetRaw("data", `[{"profile":"`+profile+`"}]`).Str
	filter := Body{}.SetRaw("filter", `{"profile":"`+profile+`"}`).Str
	// always clean up, whatever happens below; a failed delete of a missing profile is expected here
	t.Cleanup(func() { _, _ = c.DeleteBody(base, del, NoResultCheck, NoLogPayload) })
	_, _ = c.DeleteBody(base, del, NoResultCheck, NoLogPayload) // start from a clean state

	// create: a job object that says JOB_COMPLETED
	res, err := c.Post(base, create, NoLogPayload)
	require.NoError(t, err)
	assert.Equal(t, "JOB_COMPLETED", res.Get("state").String())

	// creating it again is a failure reported inside a 200: the library turns it into an error
	_, err = c.Post(base, create, NoLogPayload)
	require.Error(t, err)
	assert.Equal(t, 200, StatusCode(err))
	assert.True(t, strings.Contains(err.Error(), "already exists"), err.Error())

	// read it back by filter: the password is masked by Crosswork
	res, err = c.Post(base+"/query", filter)
	require.NoError(t, err)
	assert.Equal(t, profile, res.Get("data.0.profile").String())
	assert.Equal(t, "******", res.Get("data.0.user_pass.0.password").String())

	// delete with a request body (DELETE carries a body on these endpoints)
	res, err = c.DeleteBody(base, del, NoLogPayload)
	require.NoError(t, err)
	assert.Equal(t, "JOB_COMPLETED", res.Get("state").String())

	// gone: reading returns no data, and deleting again is a reported failure
	res, err = c.Post(base+"/query", filter)
	require.NoError(t, err)
	assert.False(t, res.Get("data").Exists())
	_, err = c.DeleteBody(base, del, NoLogPayload)
	assert.Error(t, err)
}

// sessionsOf counts the active sessions of one user.
func sessionsOf(t *testing.T, c *Client, user string) int {
	t.Helper()
	res, err := c.Get("/crosswork/aaa/v1/activeSessions")
	if err != nil {
		t.Skipf("the active sessions are not readable here: %v", err)
	}
	n := 0
	for _, s := range res.Array() {
		if s.Get("UserName").String() == user {
			n++
		}
	}
	return n
}

// Logging in adds one active session and Logout takes it away again. It creates a session for a
// moment, so like the other tests that change the cluster it needs CW_WRITE=1.
func TestLiveClientLogout_SessionCountReturnsToStart(t *testing.T) {
	if os.Getenv("CW_WRITE") != "1" {
		t.Skip("set CW_WRITE=1 to run the live test that opens and closes a session")
	}
	observer := liveClient(t)
	user := os.Getenv("CW_USERNAME")
	before := sessionsOf(t, &observer, user) // includes the observer's own session

	worker := newLiveClient(t)
	require.NoError(t, worker.Login())
	assert.Equal(t, before+1, sessionsOf(t, &observer, user), "Login adds one session")

	require.NoError(t, worker.Logout())
	assert.Equal(t, before, sessionsOf(t, &observer, user), "Logout takes it away again")

	// the worker can be used again, and logs in again
	_, err := worker.Get("/crosswork/sso/v2/userInfo")
	require.NoError(t, err)
	assert.Equal(t, before+1, sessionsOf(t, &observer, user))
	require.NoError(t, worker.Logout())
	assert.Equal(t, before, sessionsOf(t, &observer, user))
}
