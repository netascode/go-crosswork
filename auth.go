package crosswork

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Crosswork authenticates in two steps: a username and password buy a Ticket Granting
// Ticket (TGT), and the TGT is exchanged for a JWT that is sent as a Bearer token.

const (
	ticketPath = "/crosswork/sso/v1/tickets"
	jwtPath    = "/crosswork/sso/v2/tickets/jwt"
	// refreshWindow is how long before its expiry a JWT is refreshed (the JWT carries `exp`).
	refreshWindow = 60 * time.Second
	// reauthGuard: a 403 on a token minted less than this long ago is a real permission
	// denial, not a dead token, so it does not trigger a new login.
	reauthGuard = 30 * time.Second
)

// session is the mutable authentication state shared by all copies of a Client.
type session struct {
	mu     sync.RWMutex
	token  string
	exp    time.Time
	minted time.Time
	tgt    string
	static bool // WithToken: the token is supplied, never refreshed

	authMu  sync.Mutex // one login at a time
	writeMu sync.Mutex // UseMutex

	keyMu sync.Mutex               // guards keyed
	keyed map[string]chan struct{} // UseMutexKey: one lock for each key, created on first use

	change authChange // the writes that restart the authentication service, and the wait for them
}

// lockKey takes the lock with the given name and returns the function that releases it. The wait ends with the error
// of the context when the context is cancelled first. The lock is a channel with room for one value, so a waiting
// request can give up.
func (s *session) lockKey(ctx context.Context, key string) (func(), error) {
	s.keyMu.Lock()
	if s.keyed == nil {
		s.keyed = map[string]chan struct{}{}
	}
	ch, ok := s.keyed[key]
	if !ok {
		ch = make(chan struct{}, 1)
		s.keyed[key] = ch
	}
	s.keyMu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *session) current() (token string, exp time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token, s.exp
}

// snapshot returns the current token and its age from one read, so a login that finishes
// between two separate reads cannot make an old rejection look like it belongs to a new token.
func (s *session) snapshot() (token string, age time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token, time.Since(s.minted)
}

func (s *session) set(tgt, token string) {
	exp, ok := jwtExpiry(token)
	if !ok {
		exp = time.Now().Add(24 * time.Hour) // unknown: rely on the reactive path
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tgt, s.token, s.exp, s.minted = tgt, token, exp, time.Now()
}

// clear forgets the ticket and the token, so the next call logs in again.
func (s *session) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tgt, s.token, s.exp, s.minted = "", "", time.Time{}, time.Time{}
}

func (s *session) ticket() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tgt
}

// WithToken uses a pre-provisioned JWT instead of a username and password.
// The client never logs in or refreshes; a rejected token is returned as an error.
func WithToken(jwt string) func(*Client) {
	return func(client *Client) {
		exp, ok := jwtExpiry(jwt)
		if !ok {
			exp = time.Now().Add(24 * time.Hour)
		}
		client.s.token, client.s.exp, client.s.minted, client.s.static = jwt, exp, time.Now(), true
	}
}

// jwtExpiry reads the `exp` claim of a JWT without verifying its signature.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

// Login authenticates to Crosswork with the username and password and caches the ticket and JWT.
func (client *Client) Login() error {
	if client.s.static {
		return errors.New("the client uses a pre-provisioned token and cannot log in")
	}
	client.s.authMu.Lock()
	defer client.s.authMu.Unlock()
	return client.loginLocked()
}

// Authenticate makes sure a valid token is available: it logs in if there is none, and
// refreshes the JWT shortly before it expires, first from the cached ticket and, if the
// ticket was rejected, with a full login.
func (client *Client) Authenticate() error {
	tok, exp := client.s.current()
	if client.s.static {
		if tok == "" {
			return errors.New("no token configured")
		}
		return nil
	}
	if tok != "" && time.Until(exp) > refreshWindow {
		return nil
	}
	client.s.authMu.Lock()
	defer client.s.authMu.Unlock()
	if tok, exp = client.s.current(); tok != "" && time.Until(exp) > refreshWindow {
		return nil // someone else refreshed while we waited
	}
	if tgt := client.s.ticket(); tgt != "" && tok != "" {
		if jwt, err := client.requestJWT(client.context(), tgt, normalLogin); err == nil {
			client.s.set(tgt, jwt)
			return nil
		}
		log.Printf("[DEBUG] Refreshing the JWT from the cached ticket failed, logging in again")
	}
	return client.loginLocked()
}

// reauth replaces a token the server rejected. It does a full login, unless another
// request has already replaced that token.
func (client *Client) reauth(failed string) error {
	client.s.authMu.Lock()
	defer client.s.authMu.Unlock()
	if tok, _ := client.s.current(); tok != "" && tok != failed {
		return nil
	}
	return client.loginLocked()
}

// Logout ends the Crosswork session that Login or the first call created, by deleting its
// ticket. Crosswork allows a limited number of sessions per user and counts one per ticket,
// so a program that logs in more than once (every command run, every test) should call
// Logout when it is done; an unclosed session counts until it times out.
//
// Logout does nothing, and returns nil, when there is no session, and for a client created
// with WithToken, because the token is managed by whoever issued it. A ticket that Crosswork no
// longer knows (404, or a rejected token) counts as already ended. After a successful
// Logout the next call logs in again.
func (client *Client) Logout() error {
	if client.s.static {
		return nil
	}
	client.s.authMu.Lock()
	defer client.s.authMu.Unlock()
	tgt := client.s.ticket()
	if tgt == "" {
		return nil
	}
	ctx := client.context()
	token, exp := client.s.current()
	if token == "" || time.Until(exp) <= refreshWindow {
		// the delete needs a valid bearer token; a new JWT from the ticket costs no session
		jwt, err := client.requestJWT(ctx, tgt, normalLogin)
		if err != nil {
			if StatusCode(err) == http.StatusNotFound {
				client.s.clear()
				return nil
			}
			return err
		}
		client.s.set(tgt, jwt)
		token = jwt
	}
	status, body, err := client.deleteTicket(ctx, tgt, token)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300, status == http.StatusNotFound, status == http.StatusUnauthorized, status == http.StatusForbidden:
		client.s.clear()
		log.Printf("[DEBUG] Logout successful")
		return nil
	}
	log.Printf("[ERROR] Logout failed: StatusCode %d", status)
	return client.formErrorMethod(http.MethodDelete, ticketPath+"/"+tgt, status, body)
}

// deleteTicket sends the logout call. Like the login calls it bypasses Do, so it never
// triggers re-authentication, and it is not retried.
func (client *Client) deleteTicket(ctx context.Context, tgt, token string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, client.Url+ticketPath+"/"+url.PathEscape(tgt), nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if client.UserAgent != "" {
		req.Header.Set("User-Agent", client.UserAgent)
	}
	log.Printf("[DEBUG] HTTP Request: DELETE %s", req.URL.Path[:strings.LastIndex(req.URL.Path, "/")+1])
	res, err := client.HttpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := readAll(res)
	return res.StatusCode, string(body), err
}

func (client *Client) loginLocked() error {
	ctx := client.context()
	tgt, err := client.requestTGT(ctx)
	if err != nil {
		log.Printf("[ERROR] Authentication failed: %v", err)
		return err
	}
	jwt, err := client.requestJWT(ctx, tgt, normalLogin)
	if err != nil {
		log.Printf("[ERROR] Token retrieval failed: %v", err)
		return err
	}
	client.s.set(tgt, jwt)
	log.Printf("[DEBUG] Authentication successful")
	return nil
}

// requestTGT exchanges the username and password for a Ticket Granting Ticket. The
// ticket is the response body; the Location header points at an internal host and is ignored.
func (client *Client) requestTGT(ctx context.Context) (string, error) {
	status, body, err := client.postForm(ctx, ticketPath, url.Values{"username": {client.usr}, "password": {client.pwd}}, normalLogin)
	if err != nil {
		return "", err
	}
	tgt := strings.TrimSpace(body)
	if status != http.StatusCreated && status != http.StatusOK || !strings.HasPrefix(tgt, "TGT-") {
		return "", client.formError(ticketPath, status, body)
	}
	return tgt, nil
}

// requestJWT exchanges a ticket for a JWT. Crosswork validates the path of `service` only. The call says how the
// request reacts to the restart of the authentication service.
func (client *Client) requestJWT(ctx context.Context, tgt string, call loginCall) (string, error) {
	status, body, err := client.postForm(ctx, jwtPath, url.Values{"tgt": {tgt}, "service": {client.JWTService}}, call)
	if err != nil {
		return "", err
	}
	jwt := strings.TrimSpace(body)
	if status != http.StatusOK || strings.Count(jwt, ".") != 2 {
		return "", client.formError(jwtPath, status, body)
	}
	return jwt, nil
}

func (client *Client) formError(path string, status int, body string) error {
	return client.formErrorMethod(http.MethodPost, path, status, body)
}

func (client *Client) formErrorMethod(method, path string, status int, body string) error {
	msg, code, rc := parseError(body)
	return &APIError{Method: method, URL: client.Url + path, StatusCode: status, Message: msg, Code: code, Body: body, RestconfErrors: rc}
}

// The restart of the authentication service.
//
// A change to the remote authentication servers or the SSO service providers makes Crosswork restart its
// authentication service. Measured on a lab, the restart takes about 95 seconds after the first of a group of
// writes: for the first 40 seconds every ticket and JWT request answers HTTP 500 with the message
// authRestartMessage, then for about 55 seconds a request is held without an answer, and then logins work again.
// A token that was issued before the change keeps working, so only the login calls are affected. A later write
// inside the first seconds does not lengthen the restart; one made after it began to hold requests starts a new one.

const (
	// authChangeLockKey is the lock of the writes that restart the authentication service (see lockWrite).
	authChangeLockKey = "go-crosswork/auth-change"
	// authWaitLockKey is the lock of the shared wait that follows such writes (see awaitAuthChange).
	authWaitLockKey = "go-crosswork/auth-wait"
	// authRestartMessage is the error Crosswork answers a login call with while the service restarts. A 500
	// with this message is the only failure of a login call that is waited out.
	authRestartMessage = "There was a problem proxying the request"
)

// loginTiming is how a login call waits for the restart of the authentication service to end.
type loginTiming struct {
	// retryInterval is the pause between two attempts of a login call that finds the service restarting.
	retryInterval time.Duration
	// retryBudget is how long a login call keeps trying after the first sign of the restart.
	retryBudget time.Duration
	// callTimeout is the longest wait for the answer to one login request once the restart is known or the
	// request is a poll. The held requests of the restart are answered after about a minute, so this is only
	// reached when the controller does not answer at all; the other login calls keep the timeout of the client.
	callTimeout time.Duration
}

// loginWindow is the timing of the login calls that run into the restart. Tests shorten it.
var loginWindow = loginTiming{retryInterval: 5 * time.Second, retryBudget: 300 * time.Second, callTimeout: 300 * time.Second}

// loginCall says how a login request (ticket or JWT) reacts to the restart of the authentication service.
type loginCall struct {
	// waitRestart makes the call retry, within loginWindow.retryBudget, when Crosswork answers as it does during the
	// restart, so a login that starts inside the window succeeds when the window ends instead of failing.
	waitRestart bool
	// longTimeout gives the request loginWindow.callTimeout from the start, for a caller that polls.
	longTimeout bool
}

var (
	// normalLogin is every login of the client: it waits out the restart.
	normalLogin = loginCall{waitRestart: true}
	// pollLogin is a request of the wait that follows a write (WaitForLogin). The wait does its own polling, so the
	// request does not retry, but it may be held for as long as the restart holds it.
	pollLogin = loginCall{longTimeout: true}
)

// isAuthRestart reports whether an answer to a login call is the one Crosswork gives while its authentication
// service restarts. Any other 500, and any 4xx such as a rejected password, is a real failure.
func isAuthRestart(status int, body string) bool {
	return status == http.StatusInternalServerError && strings.Contains(body, authRestartMessage)
}

// isHeld reports whether a request failed because the answer did not come in time, as opposed to a failure to
// connect: the restart holds the requests it receives.
func isHeld(err error) bool {
	var ne net.Error
	return err != nil && !isPreSend(err) && errors.As(err, &ne) && ne.Timeout()
}

// restartWait paces a login call that finds the service restarting. The first sign of the restart starts the budget.
type restartWait struct{ deadline time.Time }

// seen reports whether the call has found the restart already.
func (w *restartWait) seen() bool { return !w.deadline.IsZero() }

// pause waits for the next attempt. It returns false when the budget is used up or the context ended.
func (w *restartWait) pause(ctx context.Context) bool {
	if !w.seen() {
		w.deadline = time.Now().Add(loginWindow.retryBudget)
	}
	if time.Until(w.deadline) <= loginWindow.retryInterval {
		return false
	}
	log.Printf("[WARNING] Crosswork is restarting its authentication service, trying the login again in %s", loginWindow.retryInterval)
	return sleep(ctx, loginWindow.retryInterval)
}

// postForm sends a form-encoded POST for the login calls. These bypass Do, so they never
// trigger re-authentication and never log the payload. Network errors, 429 and 502-504 are retried
// with a backoff; a 4xx (bad password, locked account) is returned at once, because retrying a rejected
// password could lock the account. When the call waits for the restart (loginCall.waitRestart), the
// answer of the restart (isAuthRestart) and a request that is held (isHeld) are retried as well, within the
// budget and with the longer timeout, and do not use up the retries of the other failures. Any other 500 is
// returned at once.
func (client *Client) postForm(ctx context.Context, path string, form url.Values, call loginCall) (int, string, error) {
	var restart restartWait
	attempts := 0
	for {
		timeout := time.Duration(0)
		if call.longTimeout || restart.seen() {
			timeout = loginWindow.callTimeout
		}
		status, body, err := client.sendForm(ctx, path, form, timeout)
		if err != nil && ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		if call.waitRestart && (isAuthRestart(status, body) || isHeld(err)) {
			if restart.pause(ctx) {
				continue
			}
			if ctx.Err() != nil {
				return 0, "", ctx.Err()
			}
			return status, body, err // the restart did not end within the budget
		}
		if err == nil && status != http.StatusTooManyRequests && (status < 502 || status > 504) {
			return status, body, nil
		}
		if client.backoff(ctx, attempts) {
			attempts++
			continue
		}
		// retries are used up: an error is returned as it is, and so is the status of 429 and 502-504, from which
		// the caller builds the APIError
		return status, body, err
	}
}

// sendForm makes one attempt of postForm. A timeout above zero replaces the timeout of the client for this request.
func (client *Client) sendForm(ctx context.Context, path string, form url.Values, timeout time.Duration) (int, string, error) {
	ctx, httpClient, cancel := withTimeout(ctx, client.HttpClient, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.Url+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/plain, application/json")
	if client.UserAgent != "" {
		req.Header.Set("User-Agent", client.UserAgent)
	}
	log.Printf("[DEBUG] HTTP Request: POST %s (payload not logged)", req.URL)
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := readAll(res)
	if err != nil {
		return 0, "", err
	}
	return res.StatusCode, string(body), nil
}

// loginWait holds the timing of the wait after a write.
type loginWait struct{ settle, interval, timeout time.Duration }

// authChangeWait is how the wait after a write that restarts the authentication service runs (see awaitAuthChange
// and WaitForLogin). Crosswork says a change can take up to three minutes to apply, so the wait allows that
// long. The window opens a few seconds after the write, so the wait starts with a pause.
var authChangeWait = loginWait{settle: 15 * time.Second, interval: 10 * time.Second, timeout: 180 * time.Second}

// loginConfirmations is the number of requests in a row that must succeed before WaitForLogin
// reports that Crosswork accepts logins again.
const loginConfirmations = 2

// authChange records the writes that restart the authentication service, so that all the writers of one apply share a
// single wait for the restart instead of each waiting on its own.
type authChange struct {
	mu       sync.Mutex
	writes   uint64    // number of writes made
	last     time.Time // time of the latest write
	settled  uint64    // the writes up to this number are covered by a wait that succeeded
	failedAt uint64    // the writes up to this number are covered by a wait that failed with err
	err      error
	wake     chan struct{} // closed, and replaced, each time a write is noted or a wait finishes
}

// signal wakes the requests that wait for the restart to end. The caller holds mu.
func (a *authChange) signal() {
	if a.wake != nil {
		close(a.wake)
	}
	a.wake = make(chan struct{})
}

// pending reports whether a write is not covered by a finished wait yet. The caller holds mu.
func (a *authChange) pending() bool { return a.writes > max(a.settled, a.failedAt) }

// hold blocks a request that is not part of the change until logins work again. Crosswork holds some of the requests
// it receives while its authentication service restarts, and a request that is held for as long as the restart lasts
// can run into its timeout, so the client does not send one while a restart is under way. The hold ends when the wait
// for the last write has finished, whether it succeeded or failed, and in any case a little after the longest that wait
// can last, so a wait that never ran cannot block requests for good. It ends early with the error of the context.
func (a *authChange) hold(ctx context.Context) error {
	for {
		a.mu.Lock()
		if !a.pending() {
			a.mu.Unlock()
			return nil
		}
		until := a.last.Add(authChangeWait.settle + authChangeWait.timeout + authChangeWait.interval)
		if a.wake == nil {
			a.wake = make(chan struct{})
		}
		wake := a.wake
		a.mu.Unlock()
		d := time.Until(until)
		if d <= 0 {
			return nil
		}
		timer := time.NewTimer(d)
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return nil
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// note records a write and returns its number.
func (a *authChange) note() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writes++
	a.last = time.Now()
	a.signal()
	return a.writes
}

// outcome returns the result of a finished wait that covers write n; done is false when none does yet.
func (a *authChange) outcome(n uint64) (done bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.settled >= n:
		return true, nil
	case a.failedAt >= n:
		return true, a.err
	}
	return false, nil
}

// round returns the number of the latest write, which the next wait covers, and the time of that write.
func (a *authChange) round() (covers uint64, last time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writes, a.last
}

// finish records the result of a wait that covered the writes up to covers.
func (a *authChange) finish(covers uint64, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.settled = max(a.settled, covers)
	} else if covers > a.failedAt {
		a.failedAt, a.err = covers, err
	}
	a.signal()
}

// awaitAuthChange waits until Crosswork accepts logins again after write n (see authChange.note). The writers of a
// burst, which Crosswork treats as one restart, share the wait: the first one polls and the others find its result.
// The wait first pauses until authChangeWait.settle has passed since the latest write, so writes that land during the
// pause are covered by the same poll; a write made after the poll began gets a wait of its own, and a poll that fails after
// such a write starts again from that write. The wait runs without
// the lock of the write, so it holds up neither the next write nor any other request.
func (client *Client) awaitAuthChange(n uint64) error {
	ctx := client.context()
	unlock, err := client.s.lockKey(ctx, authWaitLockKey)
	if err != nil {
		return err
	}
	defer unlock()
	if done, err := client.s.change.outcome(n); done {
		return err
	}
	for {
		covers, err := client.settleAuthChange(ctx)
		if err != nil {
			return err
		}
		err = client.waitForLogin(ctx, 0, authChangeWait.interval, authChangeWait.timeout)
		if cerr := ctxEnded(ctx); cerr != nil { // a cancelled wait says nothing about the restart
			return cerr
		}
		if err == nil {
			client.s.change.finish(covers, nil)
			return nil
		}
		if latest, _ := client.s.change.round(); latest > covers {
			// A write made while the poll ran started another restart, and the failure may be its: wait again,
			// with the pause and the limit counted from that write.
			continue
		}
		client.s.change.finish(covers, err)
		return err
	}
}

// settleAuthChange pauses until authChangeWait.settle has passed since the latest write, which may move while it
// waits, and returns the number of that write: the poll that follows covers every write up to it.
func (client *Client) settleAuthChange(ctx context.Context) (uint64, error) {
	for {
		covers, last := client.s.change.round()
		remaining := authChangeWait.settle - time.Since(last)
		if remaining <= 0 {
			return covers, nil
		}
		if !sleep(ctx, remaining) {
			return 0, ctx.Err()
		}
	}
}

// WaitForLogin waits until Crosswork accepts a new JWT request from the ticket of the session
// again. Some changes (the remote authentication servers and the SSO services) make the
// authentication service restart: for a minute or two after the change every ticket and
// JWT request fails or is held, while calls with a token that was already issued keep working.
// A caller that has made such a change waits with WaitForLogin before it goes on, so the next
// login, the token refresh and the logout do not fail inside that window. The client already
// does this after a PUT to those paths, so a program that only uses Put needs no call of its own.
//
// WaitForLogin first pauses for settle, because the window opens a few seconds after the change
// and a request made at once still succeeds. It then asks for a JWT every interval and returns nil
// after two requests in a row succeed. When timeout has passed since the call began without that,
// it returns an error that wraps the last failure. The request uses the existing ticket, so it opens no
// extra session; a ticket that Crosswork no longer knows is replaced by a login. Nothing is
// waited for, and nil is returned, for a client created with WithToken. WaitForLogin stops
// early with the error of the context when the context of the client ends.
func (client *Client) WaitForLogin(settle, interval, timeout time.Duration) error {
	return client.waitForLogin(client.context(), settle, interval, timeout)
}

// waitForLogin is WaitForLogin for a given context. A request of the poll is held for no longer than what is left of
// timeout.
func (client *Client) waitForLogin(parent context.Context, settle, interval, timeout time.Duration) error {
	if client.s.static {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := client.Authenticate(); err != nil {
		return err
	}
	if settle > 0 && !sleep(ctx, settle) {
		return pollEnded(parent, timeout, nil)
	}
	var last error
	confirmed := 0
	for {
		if err := client.probeLogin(ctx); err == nil {
			if confirmed++; confirmed >= loginConfirmations {
				return nil
			}
		} else {
			confirmed = 0
			if ctx.Err() == nil || last == nil { // a request cut off by the deadline does not replace the last answer
				last = err
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || !sleep(ctx, min(interval, remaining)) {
			return pollEnded(parent, timeout, last)
		}
	}
}

// ctxEnded returns the error of the context when it has ended, or when its deadline has passed but the context has not
// noticed yet (the timer that ends it can fire a moment after the deadline), and nil otherwise.
func ctxEnded(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
		return context.DeadlineExceeded
	}
	return nil
}

// pollEnded is the error of a wait that ran out: the error of the caller's context when that ended first, and
// otherwise one that wraps the last failure of the poll.
func pollEnded(parent context.Context, timeout time.Duration, last error) error {
	if err := ctxEnded(parent); err != nil {
		return err
	}
	if last == nil {
		last = errors.New("the requests did not succeed twice in a row")
	}
	return fmt.Errorf("Crosswork had not accepted a new login after %s: %w", timeout, last)
}

// probeLogin asks for a JWT from the ticket of the session and keeps it; without a ticket, or when
// Crosswork no longer knows the ticket, it logs in.
func (client *Client) probeLogin(ctx context.Context) error {
	client.s.authMu.Lock()
	defer client.s.authMu.Unlock()
	tgt := client.s.ticket()
	if tgt == "" {
		return client.loginLocked()
	}
	jwt, err := client.requestJWT(ctx, tgt, pollLogin)
	if err == nil {
		client.s.set(tgt, jwt)
		return nil
	}
	if StatusCode(err) == http.StatusNotFound {
		return client.loginLocked()
	}
	return err
}
