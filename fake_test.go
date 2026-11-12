package crosswork

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCW imitates the parts of Crosswork the client talks to: the SSO ticket and JWT
// calls, a gateway that answers 403 (or 401, or a redirect to the login page) for a
// dead token, and an API handler the tests can replace. It runs over TLS with a
// self-signed certificate, like a real deployment.
type fakeCW struct {
	*httptest.Server
	mu          sync.Mutex
	password    string
	lifetime    time.Duration // JWT lifetime
	deadStatus  int           // 403 (default), 401 or 302
	rejectAll   bool          // reject every token, even freshly minted ones
	valid       map[string]bool
	tgts        map[string]bool
	seq         int
	logins      int // ticket calls
	mints       int // jwt calls
	ticketCalls int
	reqs        []recorded
	api         func(w http.ResponseWriter, r *http.Request, n int)
	calls       map[string]int
	logoutCode  int // answer for DELETE tickets/{tgt}; 0 means the real behaviour
}

type recorded struct {
	Method, Path, Query, Body string
	Header                    http.Header
}

func newFake(t *testing.T) *fakeCW {
	f := &fakeCW{password: "pw", lifetime: time.Hour, deadStatus: http.StatusForbidden,
		valid: map[string]bool{}, tgts: map[string]bool{}, calls: map[string]int{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCW) client(t *testing.T, mods ...func(*Client)) Client {
	c, err := NewClient(f.URL, "admin", "pw", append([]func(*Client){MaxRetries(2), BackoffMinDelay(0), BackoffMaxDelay(0)}, mods...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeCW) counts() (logins, mints int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.mints
}

func (f *fakeCW) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

// terminate imitates an admin ending the user's sessions: every token and ticket dies.
func (f *fakeCW) terminate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid, f.tgts = map[string]bool{}, map[string]bool{}
}

func (f *fakeCW) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeCW) tokenFor(lifetime time.Duration) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	f.seq++
	return enc(map[string]string{"alg": "HS512"}) + "." + enc(map[string]any{"sub": "admin", "jti": f.seq, "exp": time.Now().Add(lifetime).Unix()}) + ".sig"
}

// sessions is the number of tickets the server still holds, which is what Crosswork counts
// against the per-user limit.
func (f *fakeCW) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tgts)
}

// logout handles DELETE /crosswork/sso/v1/tickets/{tgt}; the caller holds f.mu.
func (f *fakeCW) logout(w http.ResponseWriter, r *http.Request) {
	tgt := strings.TrimPrefix(r.URL.Path, "/crosswork/sso/v1/tickets/")
	auth := r.Header.Get("Authorization")
	switch {
	case f.logoutCode != 0:
		f.mu.Unlock()
		w.WriteHeader(f.logoutCode)
	case auth == "":
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"Authorization header is missing"}`)
	case !f.valid[strings.TrimPrefix(auth, "Bearer ")]:
		f.mu.Unlock()
		w.WriteHeader(f.deadStatus)
	case !f.tgts[tgt]:
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	default:
		delete(f.tgts, tgt)
		f.valid = map[string]bool{} // the tokens minted from the ticket die with it
		f.mu.Unlock()
	}
}

func (f *fakeCW) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body)) // handlers may read it again
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Clone()})
	f.calls[r.URL.Path]++
	n := f.calls[r.URL.Path]
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/crosswork/sso/v1/tickets/") {
		f.logout(w, r)
		return
	}
	switch r.URL.Path {
	case "/crosswork/sso/v1/tickets":
		f.ticketCalls++
		form, _ := url.ParseQuery(string(body))
		if form.Get("username") != "admin" || form.Get("password") != f.password {
			f.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"authentication_exceptions":["Invalid credentials"]}`)
			return
		}
		f.logins++
		tgt := fmt.Sprintf("TGT-%d-abc-cas", f.logins)
		f.tgts[tgt] = true
		f.mu.Unlock()
		w.Header().Set("Location", "https://localhost:5489/crosswork/sso/v1/tickets/"+tgt)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, tgt)
		return
	case "/crosswork/sso/v2/tickets/jwt":
		form, _ := url.ParseQuery(string(body))
		svc, _ := url.Parse(form.Get("service"))
		switch {
		case !f.tgts[form.Get("tgt")]:
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "%s TGT could not be found or is considered invalid", form.Get("tgt"))
		case svc == nil || svc.Path != "/app-dashboard":
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		default:
			f.mints++
			tok := f.tokenFor(f.lifetime)
			f.valid[tok] = true
			f.mu.Unlock()
			fmt.Fprint(w, tok)
		}
		return
	}
	ok := f.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] && !f.rejectAll
	dead := f.deadStatus
	f.mu.Unlock()
	if !ok {
		switch dead {
		case http.StatusFound:
			w.Header().Set("Location", "https://other.example.com:30603/crosswork/sso/login?client_name=x")
		}
		w.WriteHeader(dead)
		fmt.Fprint(w, `{"error":"Your session has ended. Log into the system again to continue."}`)
		return
	}
	if f.api != nil {
		f.api(w, r, n)
		return
	}
	fmt.Fprint(w, `{"ok":true}`)
}
