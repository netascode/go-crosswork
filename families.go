package crosswork

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Crosswork-specific behaviour that depends on the request path lives here, the same way
// Callers pass full paths, so
// there are no base-path constants: the library only recognises a few path shapes.

const (
	jsonType = "application/json"
	// jsonBodyTyp is sent as Content-Type for JSON bodies: the aaa gateway answers 405 to a bare application/json.
	jsonBodyTyp = "application/json; charset=UTF-8"
	restconfTyp = "application/yang-data+json"
	patchType   = "application/yang-patch+json"
)

// matchPath returns the request path normalised for matching: runs of slashes are
// collapsed (some documented paths contain //) and anything before /crosswork/ is
// dropped, so a configured URL path prefix does not matter.
func matchPath(u *url.URL) string {
	p := collapseSlashes(u.Path)
	if i := strings.Index(p, "/crosswork/"); i > 0 {
		p = p[i:]
	}
	return p
}

// isRestconf reports whether a path belongs to a RESTCONF API (they contain /restconf/).
func isRestconf(p string) bool {
	return strings.Contains(p, "/restconf/") || strings.HasSuffix(p, "/restconf")
}

// readOnlyPost reports whether a POST path is a read: Crosswork queries are POSTs.
func readOnlyPost(p string) bool {
	p = strings.TrimSuffix(p, "/")
	for _, s := range []string{"/query", "/querybrief", "/count"} {
		if strings.HasSuffix(p, s) {
			return true
		}
	}
	return false
}

// dgManagerPrefix is the path of the Data Gateway manager, which answers 400 ("received
// unexpected Content-Type") to the charset form of the JSON content type.
const dgManagerPrefix = "/crosswork/dg-manager/"

// applyDefaultHeaders sets Content-Type and Accept from the path unless a request mod
// (or YangPatch) already set them: yang-data+json for RESTCONF paths, a bare application/json
// for the Data Gateway manager, JSON with a charset otherwise.
func applyDefaultHeaders(req *Req) {
	t, ct := jsonType, jsonBodyTyp
	p := matchPath(req.HttpReq.URL)
	switch {
	case isRestconf(p):
		t, ct = restconfTyp, restconfTyp
	case strings.HasPrefix(p, dgManagerPrefix):
		ct = jsonType
	}
	if req.HttpReq.Header.Get("Content-Type") == "" {
		req.HttpReq.Header.Set("Content-Type", ct)
	}
	if req.HttpReq.Header.Get("Accept") == "" {
		req.HttpReq.Header.Set("Accept", t)
	}
}

// canRetryAmbiguous reports whether a request may be retried after a failure whose
// outcome is unknown (connection reset after sending, timeout, 502/503/504). Reads,
// read-only POSTs and PUT are repeatable. POST creates, PATCH and DELETE are not: a
// repeated DLM create fails ("already exists") and a repeated DLM delete fails, which
// would turn a success into a failure.
func canRetryAmbiguous(req Req) bool {
	if req.RetryNonIdempotent {
		return true
	}
	switch req.HttpReq.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut:
		return true
	case http.MethodPost:
		return req.ReadOnly || readOnlyPost(matchPath(req.HttpReq.URL))
	}
	return false
}

// isAuthFailure reports whether a response means the token is no longer accepted.
// Crosswork answers a dead token with 403 (not 401), and some paths with a redirect to
// the SSO login page. The response body is never inspected.
func isAuthFailure(status int, h http.Header) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		u, err := url.Parse(h.Get("Location"))
		return err == nil && strings.HasPrefix(collapseSlashes(u.Path), "/crosswork/sso/login")
	}
	return false
}

// nsoWriteTimeout is the request timeout of a write under the NSO proxy when the caller sets
// none: the call includes the device push, which can outlast the 60 second client default.
const nsoWriteTimeout = 120 * time.Second

// defaultTimeout returns the timeout for a request that has no Timeout of its own, or zero to
// use the client's timeout.
func defaultTimeout(req Req) time.Duration {
	if req.HttpReq.Method != http.MethodGet && req.HttpReq.Method != http.MethodHead &&
		strings.HasPrefix(matchPath(req.HttpReq.URL), nsoProxyPrefix) {
		return nsoWriteTimeout
	}
	return 0
}
