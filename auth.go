package crosswork

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
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
		if jwt, err := client.requestJWT(client.context(), tgt); err == nil {
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
		jwt, err := client.requestJWT(ctx, tgt)
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
	jwt, err := client.requestJWT(ctx, tgt)
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
	status, body, err := client.postForm(ctx, ticketPath, url.Values{"username": {client.usr}, "password": {client.pwd}})
	if err != nil {
		return "", err
	}
	tgt := strings.TrimSpace(body)
	if status != http.StatusCreated && status != http.StatusOK || !strings.HasPrefix(tgt, "TGT-") {
		return "", client.formError(ticketPath, status, body)
	}
	return tgt, nil
}

// requestJWT exchanges a ticket for a JWT. Crosswork validates the path of `service` only.
func (client *Client) requestJWT(ctx context.Context, tgt string) (string, error) {
	status, body, err := client.postForm(ctx, jwtPath, url.Values{"tgt": {tgt}, "service": {client.JWTService}})
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

// postForm sends a form-encoded POST for the login calls. These bypass Do, so they never
// trigger re-authentication and never log the payload. Only network errors, 429 and
// 502-504 are retried; a 4xx (bad password, locked account) is returned at once, because
// retrying a rejected password could lock the account.
func (client *Client) postForm(ctx context.Context, path string, form url.Values) (int, string, error) {
	for attempts := 0; ; attempts++ {
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
		res, err := client.HttpClient.Do(req)
		var body []byte
		if err == nil {
			body, err = readAll(res)
		}
		retry := false
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return 0, "", ctx.Err()
			}
			retry = true
		case res.StatusCode == 429 || (res.StatusCode >= 502 && res.StatusCode <= 504):
			retry = true
		default:
			return res.StatusCode, string(body), nil
		}
		if retry && client.backoff(ctx, attempts) {
			continue
		}
		if err != nil {
			return 0, "", err
		}
		// retries exhausted on 429/502-504: hand the status to the caller, which builds the APIError
		return res.StatusCode, string(body), nil
	}
}
