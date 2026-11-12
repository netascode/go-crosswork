package crosswork

import (
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	c, err := NewClient("https://cnc.example.com:30603/", "admin", "pw")
	require.NoError(t, err)
	assert.Equal(t, "https://cnc.example.com:30603", c.Url, "the trailing slash is trimmed and nothing else is added")
	assert.Equal(t, DefaultMaxRetries, c.MaxRetries)
	assert.Equal(t, DefaultBackoffMinDelay, c.BackoffMinDelay)
	assert.Equal(t, DefaultBackoffMaxDelay, c.BackoffMaxDelay)
	assert.Equal(t, DefaultBackoffDelayFactor, c.BackoffDelayFactor)
	assert.Equal(t, 60*time.Second, c.HttpClient.Timeout)
	assert.Equal(t, "https://cnc.example.com:30603/app-dashboard", c.JWTService)
	assert.Nil(t, c.HttpClient.Jar, "no cookie jar: every call carries only the bearer token")
}

func TestNewClient_WithoutCredentials(t *testing.T) {
	_, err := NewClient("https://h", "", "")
	assert.Error(t, err)
	_, err = NewClient("https://h", "admin", "")
	assert.Error(t, err)
	_, err = NewClient("https://h", "", "", WithToken("a.b.c"))
	assert.NoError(t, err, "a token replaces the username and password")
}

func TestClientOptions(t *testing.T) {
	c, err := NewClient("https://h", "u", "p",
		MaxRetries(7), BackoffMinDelay(1), BackoffMaxDelay(9), BackoffDelayFactor(1.5),
		RequestTimeout(5*time.Second), UserAgent("tf/1.0"), RetryOnErrorCodes([]int{409}), JWTService("https://x/app-dashboard"))
	require.NoError(t, err)
	assert.Equal(t, 7, c.MaxRetries)
	assert.Equal(t, 1, c.BackoffMinDelay)
	assert.Equal(t, 9, c.BackoffMaxDelay)
	assert.Equal(t, 1.5, c.BackoffDelayFactor)
	assert.Equal(t, 5*time.Second, c.HttpClient.Timeout)
	assert.Equal(t, "tf/1.0", c.UserAgent)
	assert.Equal(t, []int{409}, c.RetryOnErrorCodes)
	assert.Equal(t, "https://x/app-dashboard", c.JWTService)
}

// The fake server uses a self-signed certificate, like many real Crosswork deployments.
func TestClientInsecure_DefaultAndSwitch(t *testing.T) {
	f := newFake(t)

	c := f.client(t) // default: self-signed certificates are accepted
	_, err := c.Get(apiPath)
	assert.NoError(t, err, "default accepts a self-signed certificate")

	c = f.client(t, Insecure(true))
	_, err = c.Get(apiPath)
	assert.NoError(t, err)

	before := f.callCount("/crosswork/sso/v1/tickets")
	c = f.client(t, Insecure(false))
	_, err = c.Get(apiPath)
	require.Error(t, err, "Insecure(false) really verifies the certificate")
	var unknown x509.UnknownAuthorityError
	assert.True(t, errors.As(err, &unknown), "got %v", err)
	assert.Equal(t, before, f.callCount("/crosswork/sso/v1/tickets"), "the password is never sent to a server whose certificate failed verification")
}

// userAgents returns the User-Agent of every request the fake server saw for paths with the given prefix.
func userAgents(f *fakeCW, prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		if strings.HasPrefix(r.Path, prefix) {
			out = append(out, r.Header.Get("User-Agent"))
		}
	}
	return out
}

func TestClientUserAgent_Login(t *testing.T) {
	f := newFake(t)
	c := f.client(t, UserAgent("CrossworkTerraform/9.9"))
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	got := userAgents(f, "/crosswork/sso/")
	assert.Len(t, got, 2, "the ticket call and the JWT call")
	for _, ua := range got {
		assert.Equal(t, "CrossworkTerraform/9.9", ua)
	}
}

func TestClientUserAgent_API(t *testing.T) {
	f := newFake(t)
	c := f.client(t, UserAgent("CrossworkTerraform/9.9"))
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"CrossworkTerraform/9.9"}, userAgents(f, apiPath))
}

func TestClientJWTService_DefaultIsAccepted(t *testing.T) {
	f := newFake(t)
	c := f.client(t) // the default <url>/app-dashboard is accepted
	_, err := c.Get(apiPath)
	require.NoError(t, err)
}

func TestClientJWTService_BadServiceIsRejected(t *testing.T) {
	f := newFake(t)
	bad := f.client(t, JWTService("https://x.example.com/"))
	_, err := bad.Get(apiPath)
	require.Error(t, err)
	assert.Equal(t, 500, StatusCode(err), "the server rejects a service without the /app-dashboard path")
}

func TestClientBackoff(t *testing.T) {
	c, _ := NewClient("https://h", "u", "p", MaxRetries(2), BackoffMinDelay(0), BackoffMaxDelay(0))
	assert.True(t, c.Backoff(0))
	assert.True(t, c.Backoff(1))
	assert.False(t, c.Backoff(2), "attempts >= MaxRetries stops retrying")
}

func TestClientBackoff_DelayBounds(t *testing.T) {
	c, _ := NewClient("https://h", "u", "p", BackoffMinDelay(2), BackoffMaxDelay(10), BackoffDelayFactor(3))
	for attempts := 0; attempts < 5; attempts++ {
		d := c.backoffDelay(attempts)
		assert.GreaterOrEqual(t, d, 2*time.Second)
		assert.LessOrEqual(t, d, 10*time.Second)
	}
}

func TestClientWithContext_SharesSession(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	_, err := c.Get(apiPath)
	require.NoError(t, err)
	bound := c.WithContext(t.Context())
	_, err = bound.Get(apiPath)
	require.NoError(t, err)
	logins, _ := f.counts()
	assert.Equal(t, 1, logins, "a WithContext copy reuses the token of the original")
}
