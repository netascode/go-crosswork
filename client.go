// Package crosswork is a Cisco Crosswork Network Controller REST and RESTCONF client library for Go.
package crosswork

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// Default retry behaviour.
const (
	DefaultMaxRetries         int     = 3
	DefaultBackoffMinDelay    int     = 2
	DefaultBackoffMaxDelay    int     = 60
	DefaultBackoffDelayFactor float64 = 3
	// DefaultDefaultMaxAsyncWaitTime is the default longest wait for an NSO plan, in seconds.
	DefaultDefaultMaxAsyncWaitTime int = 180
	// DefaultPlanPollInterval is the default time between two reads of an NSO plan.
	DefaultPlanPollInterval = time.Second
)

// Client is an HTTP Crosswork client.
// Always use NewClient to construct it, otherwise requests will panic.
//
// A Client is cheap to copy: all session state (token, ticket, mutexes) sits behind one
// shared pointer, so copies made with WithContext stay in sync. The request methods have
// pointer receivers, so assign the result of WithContext to a variable before calling them.
type Client struct {
	// HttpClient is the *http.Client used for API requests. Its Transport must stay a
	// *http.Transport: Insecure type-asserts it.
	HttpClient *http.Client
	// Url is the Crosswork base URL, e.g. https://cnc.example.com:30603 (without a trailing slash).
	Url string
	// Maximum number of retries
	MaxRetries int
	// Minimum delay between two retries, in seconds
	BackoffMinDelay int
	// Maximum delay between two retries, in seconds
	BackoffMaxDelay int
	// Backoff delay factor
	BackoffDelayFactor float64
	// UserAgent is the HTTP User-Agent string sent with requests.
	UserAgent string
	// RetryOnErrorCodes lists extra status codes that are retried, for calls that may be retried.
	RetryOnErrorCodes []int
	// NSOLockTimeout enables the NSO busy policy when greater than zero (see NSORetry): the
	// total seconds to wait for the NSO lock to be released. Zero (the default) disables it.
	NSOLockTimeout int
	// DefaultMaxAsyncWaitTime is the longest time in seconds to wait for an NSO plan, when a
	// request does not set MaxAsyncWaitTime. See DefaultMaxAsyncWaitTime.
	DefaultMaxAsyncWaitTime int
	// PlanPollInterval is the time between two reads of an NSO plan while waiting.
	PlanPollInterval time.Duration
	// JWTService is the `service` value sent when exchanging the ticket for a JWT.
	// The default is <Url>/app-dashboard; only the path matters to Crosswork.
	JWTService string

	usr, pwd string
	ctx      context.Context
	s        *session
}

// NewClient creates a new Crosswork HTTP client.
// Pass modifiers in to modify the behavior of the client, e.g.
//
//	client, _ := NewClient("https://cnc.example.com:30603", "user", "password", Insecure(false), RequestTimeout(120))
//
// To use a pre-provisioned JWT instead of a username and password, use WithToken.
// Self-signed certificates are accepted by default; pass
// Insecure(false) to verify them.
func NewClient(url, usr, pwd string, mods ...func(*Client)) (Client, error) {
	tr := newTransport()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	httpClient := http.Client{
		Timeout:   60 * time.Second,
		Transport: tr,
		// Crosswork answers a missing or dead token with a redirect to its SSO login page,
		// whose host is not reachable through a tunnel. Never follow redirects.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// Remove trailing slash from base URL to prevent double slashes in the final request URL
	url = strings.TrimSuffix(url, "/")

	client := Client{
		HttpClient:              &httpClient,
		Url:                     url,
		usr:                     usr,
		pwd:                     pwd,
		MaxRetries:              DefaultMaxRetries,
		BackoffMinDelay:         DefaultBackoffMinDelay,
		BackoffMaxDelay:         DefaultBackoffMaxDelay,
		BackoffDelayFactor:      DefaultBackoffDelayFactor,
		UserAgent:               "go-crosswork netascode",
		DefaultMaxAsyncWaitTime: DefaultDefaultMaxAsyncWaitTime,
		PlanPollInterval:        DefaultPlanPollInterval,
		s:                       &session{},
	}

	for _, mod := range mods {
		mod(&client)
	}
	if client.JWTService == "" {
		client.JWTService = client.Url + "/app-dashboard"
	}
	if !client.s.static && (usr == "" || pwd == "") {
		return client, errors.New("no authentication method configured, use a username and password or WithToken")
	}
	return client, nil
}

// newTransport returns a clone of http.DefaultTransport, so proxy environment variables
// keep working. Test tools such as gock replace http.DefaultTransport, so fall back to an
// equivalent transport instead of panicking on the type assertion.
func newTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// DefaultMaxAsyncWaitTime modifies the longest wait for an NSO plan from the default of 180 seconds.
func DefaultMaxAsyncWaitTime(x int) func(*Client) {
	return func(client *Client) {
		client.DefaultMaxAsyncWaitTime = x
	}
}

// PlanPollInterval modifies the time between two reads of an NSO plan from the default of one second.
func PlanPollInterval(x time.Duration) func(*Client) {
	return func(client *Client) {
		client.PlanPollInterval = x
	}
}

// Insecure determines if insecure https connections are allowed. Default value is true.
func Insecure(x bool) func(*Client) {
	return func(client *Client) {
		client.HttpClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = x
	}
}

// RequestTimeout modifies the HTTP request timeout from the default of 60 seconds.
func RequestTimeout(x time.Duration) func(*Client) {
	return func(client *Client) {
		client.HttpClient.Timeout = x
	}
}

// MaxRetries modifies the maximum number of retries from the default of 3.
func MaxRetries(x int) func(*Client) {
	return func(client *Client) {
		client.MaxRetries = x
	}
}

// BackoffMinDelay modifies the minimum delay between two retries from the default of 2 seconds.
func BackoffMinDelay(x int) func(*Client) {
	return func(client *Client) {
		client.BackoffMinDelay = x
	}
}

// BackoffMaxDelay modifies the maximum delay between two retries from the default of 60 seconds.
func BackoffMaxDelay(x int) func(*Client) {
	return func(client *Client) {
		client.BackoffMaxDelay = x
	}
}

// BackoffDelayFactor modifies the backoff delay factor from the default of 3.
func BackoffDelayFactor(x float64) func(*Client) {
	return func(client *Client) {
		client.BackoffDelayFactor = x
	}
}

// UserAgent modifies the HTTP User-Agent string.
func UserAgent(x string) func(*Client) {
	return func(client *Client) {
		client.UserAgent = x
	}
}

// RetryOnErrorCodes adds status codes that are retried, for calls that may be retried
// after an ambiguous failure (see Req.ReadOnly and Req.RetryNonIdempotent).
func RetryOnErrorCodes(x []int) func(*Client) {
	return func(client *Client) {
		client.RetryOnErrorCodes = x
	}
}

// JWTService sets the `service` value sent when exchanging the ticket for a JWT.
func JWTService(x string) func(*Client) {
	return func(client *Client) {
		client.JWTService = x
	}
}

// WithContext returns a copy of the client bound to ctx. Requests, backoff sleeps and
// logins made through the copy stop when ctx is cancelled. Session state is shared.
func (client Client) WithContext(ctx context.Context) Client {
	client.ctx = ctx
	return client
}

func (client Client) context() context.Context {
	if client.ctx != nil {
		return client.ctx
	}
	return context.Background()
}

// NewReq creates a new Req request for this client.
func (client Client) NewReq(method, uri string, body io.Reader, mods ...func(*Req)) Req {
	httpReq, err := http.NewRequestWithContext(client.context(), method, client.Url+uri, body)
	req := Req{HttpReq: httpReq, LogPayload: true, buildErr: err}
	if err != nil {
		return req
	}
	if client.UserAgent != "" {
		httpReq.Header.Set("User-Agent", client.UserAgent)
	}
	for _, mod := range mods {
		mod(&req)
	}
	return req
}

// backoffDelay is the delay before retry number attempts+1: exponential, capped and jittered.
func (client *Client) backoffDelay(attempts int) time.Duration {
	minDelay := time.Duration(client.BackoffMinDelay) * time.Second
	maxDelay := time.Duration(client.BackoffMaxDelay) * time.Second
	min := float64(minDelay)
	backoff := min * math.Pow(client.BackoffDelayFactor, float64(attempts))
	if backoff > float64(maxDelay) {
		backoff = float64(maxDelay)
	}
	backoff = (rand.Float64()/2+0.5)*(backoff-min) + min
	return time.Duration(backoff)
}

// sleep waits for d or until ctx is done. It reports whether the full wait happened.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// backoff waits following an exponential backoff algorithm and reports whether to retry.
// It stops waiting when ctx is cancelled.
func (client *Client) backoff(ctx context.Context, attempts int) bool {
	if attempts >= client.MaxRetries {
		return false
	}
	return sleep(ctx, client.backoffDelay(attempts))
}

// Backoff waits following an exponential backoff algorithm and reports whether to retry.
// It reports false once the maximum number of attempts is reached.
func (client *Client) Backoff(attempts int) bool {
	return client.backoff(client.context(), attempts)
}
