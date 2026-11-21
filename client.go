// Package crosswork is a Cisco Crosswork Network Controller REST and RESTCONF client library for Go.
package crosswork

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Default retry behaviour.
const (
	DefaultMaxRetries         int     = 3
	DefaultBackoffMinDelay    int     = 2
	DefaultBackoffMaxDelay    int     = 60
	DefaultBackoffDelayFactor float64 = 3
	// DefaultDefaultMaxAsyncWaitTime is the default longest wait for an NSO plan, in seconds.
	DefaultDefaultMaxAsyncWaitTime int = 300
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
		// whose host may not be reachable from the client. Never follow redirects.
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

// DefaultMaxAsyncWaitTime modifies the longest wait for an NSO plan from the default of 300 seconds.
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

// Do makes a request.
// Requests for Do are built outside of the client, e.g.
//
//	req := client.NewReq("GET", "/crosswork/inventory/v1/nodes/count", nil)
//	res, _ := client.Do(req)
//
// On a failed call Res still holds the parsed response body, next to the *APIError.
func (client *Client) Do(req Req) (Res, error) {
	if req.buildErr != nil {
		return Res{}, req.buildErr
	}
	var body []byte
	if req.HttpReq.Body != nil {
		body, _ = io.ReadAll(req.HttpReq.Body)
	}
	applyDefaultHeaders(&req)

	if !restartsAuth(req) {
		// Nothing but the writes that cause a restart is sent while one is under way (see authChange.hold).
		if err := client.s.change.hold(req.HttpReq.Context()); err != nil {
			return Res{}, err
		}
	}
	release, err := client.lockWrite(req)
	if err != nil {
		return Res{}, err
	}
	defer release()
	authChange := restartsAuth(req)
	ctx := req.HttpReq.Context()
	defer func() {
		log.Printf("[DEBUG] Exit from Do method: %s, %s", req.HttpReq.Method, req.HttpReq.URL)
	}()

	attempts := 0
	for {
		if !authChange {
			// Also before a repeat of the request: one that was sent just before a restart began is answered with an
			// error (a 401 from the NSO proxy, for example) and must not be sent again into the restart.
			if err := client.s.change.hold(ctx); err != nil {
				return Res{}, err
			}
		}
		if err := client.Authenticate(); err != nil {
			return Res{}, err
		}
		token, _ := client.s.current()
		status, hdr, raw, err := client.send(req, body, token)
		if err != nil {
			if ctx.Err() != nil {
				return Res{}, ctx.Err()
			}
			// Nothing was sent (dial or TLS failure): safe to retry for every method.
			// Otherwise the outcome is unknown: only reads and repeatable writes are retried.
			if (isPreSend(err) || canRetryAmbiguous(req)) && client.backoff(ctx, attempts) {
				log.Printf("[ERROR] HTTP Connection failed: %s, retries: %v", err, attempts)
				attempts++
				continue
			}
			log.Printf("[ERROR] HTTP Connection error occurred: %+v", err)
			return Res{}, err
		}
		res := gjson.ParseBytes(raw)
		if req.LogPayload {
			log.Printf("[DEBUG] HTTP Response: %s", truncate(redact(oneLine(string(raw))), 2000))
		}

		if status >= 200 && status <= 299 {
			res, err = client.finish(req, status, res, raw)
			if err == nil && req.PlanPath != "" && !req.NoWait && req.HttpReq.Method != http.MethodGet {
				err = client.waitForPlan(req)
			}
			if err == nil && authChange && req.AfterWrite == nil {
				// The write is done: note it and let go of the lock, so that the writers that follow do not wait
				// for the restart one by one and other writes are not held up. The wait is shared (awaitAuthChange).
				n := client.s.change.note()
				release()
				if werr := client.awaitAuthChange(n); werr != nil {
					err = &WaitError{Err: werr}
				}
			}
			if err == nil && req.AfterWrite != nil && req.HttpReq.Method != http.MethodGet {
				if werr := req.AfterWrite(req.HttpReq.Context(), res); werr != nil {
					err = &WaitError{Err: werr}
				}
			}
			return res, err
		}

		// Token no longer accepted: log in again once per request and retry.
		// A 403 on a token minted moments ago is a real permission denial, but only if that
		// token is still the current one: if another request already replaced it, this
		// response belongs to the old token and the request just needs the new one.
		current, tokenAge := client.s.snapshot()
		replaced := current != "" && current != token
		if isAuthFailure(status, hdr) && !client.s.static && !req.reAuthAttempted &&
			!(status == http.StatusForbidden && isAccessDenied(raw)) &&
			(replaced || !(status == http.StatusForbidden && tokenAge < reauthGuard)) {
			log.Printf("[WARNING] Received %d. Attempting to re-authenticate.", status)
			req.reAuthAttempted = true
			if err := client.reauth(token); err != nil {
				return res, err
			}
			continue
		}

		// Rate limited: the request was not processed, so it is safe for every method.
		if status == http.StatusTooManyRequests && attempts < client.MaxRetries {
			d := client.backoffDelay(attempts)
			if ra := retryAfter(hdr); ra > d {
				d = ra
			}
			log.Printf("[WARNING] HTTP Request rate limited, waiting %v seconds, Retries: %v", d.Seconds(), attempts)
			if !sleep(ctx, d) {
				return res, ctx.Err()
			}
			attempts++
			continue
		}

		// NSO is busy (lock-denied or in-use): the write was not applied, so any method may be retried.
		if attempts < client.MaxRetries && client.nsoBusy(req, status, raw) {
			log.Printf("[WARNING] NSO is busy (StatusCode %v), waiting for the datastore lock, Retries: %v", status, attempts)
			client.waitForNSOLock(ctx, req)
			if client.backoff(ctx, attempts) {
				attempts++
				continue
			}
		}

		if client.retryStatus(status) && canRetryAmbiguous(req) && client.backoff(ctx, attempts) {
			log.Printf("[ERROR] HTTP Request failed: StatusCode %v, Retries: %v", status, attempts)
			attempts++
			continue
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		err = client.apiError(req, status, string(raw), "")
		log.Printf("[ERROR] %v", err)
		return res, err
	}
}

// lockWrite takes the locks a write asks for and returns the function that lets them go again, which may be
// called more than once. A read takes none. A write to the remote authentication servers or the SSO service
// providers (restartsAuth) takes the lock of its own (authChangeLockKey) in place of the client's write lock
// that UseMutex asks for, because Crosswork then restarts its authentication service and the writes that follow
// must not wait for that. A request that names a key with UseMutexKey takes that lock as well.
func (client *Client) lockWrite(req Req) (release func(), err error) {
	var unlocks []func()
	release = func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
		unlocks = nil
	}
	if req.HttpReq.Method == http.MethodGet {
		return release, nil
	}
	lockKey := func(key string) error {
		unlock, err := client.s.lockKey(req.HttpReq.Context(), key)
		if err != nil {
			return err
		}
		unlocks = append(unlocks, unlock)
		return nil
	}
	switch {
	case restartsAuth(req):
		if err := lockKey(authChangeLockKey); err != nil {
			return release, err
		}
	case req.UseMutex:
		client.s.writeMu.Lock()
		unlocks = append(unlocks, client.s.writeMu.Unlock)
	}
	if req.MutexKey != "" {
		if err := lockKey(req.MutexKey); err != nil {
			release()
			return release, err
		}
	}
	return release, nil
}

// restartsAuth reports whether the request is a PUT that makes Crosswork restart its authentication service.
func restartsAuth(req Req) bool {
	return req.HttpReq.Method == http.MethodPut && restartsLogin(matchPath(req.HttpReq.URL))
}

// isAccessDenied reports whether a body carries the RESTCONF error-tag access-denied (RFC 8040),
// which NSO's access control returns for a user without rights. It is a structured tag, not
// message text, and it means the token is fine: logging in again would not help.
func isAccessDenied(raw []byte) bool {
	_, _, rc := parseError(string(raw))
	for _, e := range rc {
		if e.Tag == "access-denied" {
			return true
		}
	}
	return false
}

// retryStatus reports whether a status is worth retrying when the call may be retried.
// The gateway's 500 "NATS request failed" is never retried: it means the request could
// not be routed or parsed, which a retry cannot fix.
func (client *Client) retryStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return contains(intStrings(client.RetryOnErrorCodes), strconv.Itoa(status))
}

func intStrings(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// send makes one attempt and returns the status, headers and body.
func (client *Client) send(req Req, body []byte, token string) (int, http.Header, []byte, error) {
	ctx := req.HttpReq.Context()
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout(req)
	}
	ctx, httpClient, cancel := withTimeout(ctx, client.HttpClient, timeout)
	defer cancel()
	r := req.HttpReq.Clone(ctx)
	if len(body) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	} else {
		r.Body, r.ContentLength = nil, 0
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if req.LogPayload && len(body) > 0 {
		log.Printf("[DEBUG] HTTP Request: %s, %s, %s", r.Method, r.URL, truncate(redact(string(body)), 2000))
	} else {
		log.Printf("[DEBUG] HTTP Request: %s, %s", r.Method, r.URL)
	}
	res, err := httpClient.Do(r)
	if err != nil {
		return 0, nil, nil, err
	}
	raw, err := readAll(res)
	if err != nil {
		return 0, nil, nil, err
	}
	return res.StatusCode, res.Header, raw, nil
}

// withTimeout returns the context and the HTTP client for one request. With a timeout, the deadline is set on the
// context and the client gets none of its own, so the request may take longer than the timeout of the client; without
// one, the client's own timeout applies. The caller must call the returned cancel function.
func withTimeout(ctx context.Context, hc *http.Client, timeout time.Duration) (context.Context, *http.Client, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, hc, func() {}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	own := *hc
	own.Timeout = 0
	return ctx, &own, cancel
}

// finish handles a 2xx response: it turns a failure reported inside the body into an error.
func (client *Client) finish(req Req, status int, res Res, raw []byte) (Res, error) {
	if req.noResultCheck {
		return res, nil
	}
	if req.resultCheck != nil {
		if err := req.resultCheck(res); err != nil {
			if asAPIError(err) != nil {
				return res, err
			}
			return res, client.apiError(req, status, string(raw), err.Error())
		}
		return res, nil
	}
	if msg, ok := resultFailure(res); ok {
		err := client.apiError(req, status, string(raw), msg)
		if err.Code == "" {
			err.Code = statusErrorCode(res)
		}
		err.EntryStatus = entryStatuses(res)
		log.Printf("[ERROR] %v", err)
		return res, err
	}
	logUnmatched(res)
	return res, nil
}

// apiError builds the error for a response. msg overrides the message extracted from the body.
func (client *Client) apiError(req Req, status int, body, msg string) *APIError {
	m, code, rc := parseError(body)
	if msg != "" {
		m, code = msg, codeFromMessage(msg)
	}
	return &APIError{Method: req.HttpReq.Method, URL: req.HttpReq.URL.String(), StatusCode: status,
		Message: m, Code: code, Body: body, RestconfErrors: rc}
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date. "0" waits one second.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return time.Second
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// isPreSend reports whether a transport error happened before the request was sent
// (dial, DNS or TLS certificate failures), so nothing can have reached the server.
func isPreSend(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	return errors.As(err, &dns) || errors.As(err, &unknown) || errors.As(err, &host) ||
		errors.As(err, &invalid) || errors.As(err, &verify) || errors.As(err, &header)
}

// Get makes a GET request and returns a gjson result.
// The path is the full path, e.g. /crosswork/inventory/v1/nodes/count.
func (client *Client) Get(path string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodGet, path, nil, mods...))
}

// Post makes a POST request and returns a gjson result.
// Hint: Use the Body struct to easily create POST body data.
func (client *Client) Post(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodPost, path, strings.NewReader(data), mods...))
}

// Put makes a PUT request and returns a gjson result.
func (client *Client) Put(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodPut, path, strings.NewReader(data), mods...))
}

// Patch makes a PATCH request and returns a gjson result.
func (client *Client) Patch(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodPatch, path, strings.NewReader(data), mods...))
}

// Delete makes a DELETE request and returns a gjson result.
func (client *Client) Delete(path string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodDelete, path, nil, mods...))
}

// DeleteBody makes a DELETE request with a body and returns a gjson result.
// Many Crosswork DELETE operations take a request body, e.g. DLM
// DELETE /crosswork/inventory/v1/credentials and /nodes.
func (client *Client) DeleteBody(path, data string, mods ...func(*Req)) (Res, error) {
	return client.Do(client.NewReq(http.MethodDelete, path, strings.NewReader(data), mods...))
}
