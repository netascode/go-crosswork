package crosswork

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// RestconfError is one entry of a RESTCONF `errors` structure (RFC 8040).
type RestconfError struct {
	Type    string
	Tag     string
	AppTag  string
	Path    string
	Message string
	Info    string
}

// APIError is returned for every failed request: a non-2xx status, or a 2xx response
// whose body reports a failure (see ResultCheck).
//
// The Error text keeps the `HTTP Request failed: StatusCode N` prefix,
// so callers that match on it keep working.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	// Message is the human readable message extracted from the body.
	Message string
	// Code is a structured code when the API supplies one: a RESTCONF error-tag, or
	// the [ErrXxx] prefix that some DLM messages carry. It is never derived from prose.
	Code string
	// Body is the raw response body.
	Body string
	// RestconfErrors holds the parsed RESTCONF errors, if any.
	RestconfErrors []RestconfError
	// EntryStatus holds the codes of the failed entries when a 2xx answer is a list of per-entry results
	// (HTTP 207 from the Workflow Manager Solutions services), for example [404, 409]; nil otherwise.
	EntryStatus []int
}

// Error implements the error interface.
func (e *APIError) Error() string {
	s := fmt.Sprintf("HTTP Request failed: StatusCode %d", e.StatusCode)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// WaitError is returned by Do when a write was accepted (2xx) but the AfterWrite waiter of the request failed, for
// example a job that is still running at the timeout. Do returns the response of the write with it, so the caller can
// read the id that the write returned. It unwraps to the error of the waiter.
type WaitError struct {
	Err error
}

// Error implements the error interface.
func (e *WaitError) Error() string { return e.Err.Error() }

// Unwrap returns the error of the waiter.
func (e *WaitError) Unwrap() error { return e.Err }

// HasRestconfTag reports whether any RESTCONF error carries the given error-tag.
func (e *APIError) HasRestconfTag(tag string) bool {
	for _, r := range e.RestconfErrors {
		if r.Tag == tag {
			return true
		}
	}
	return false
}

func asAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// StatusCode returns the HTTP status code of an *APIError, or 0 for any other error.
func StatusCode(err error) int {
	if ae := asAPIError(err); ae != nil {
		return ae.StatusCode
	}
	return 0
}

// notFoundCodes are the structured error codes that services return, inside a 200,
// for an object that does not exist.
var notFoundCodes = map[string]bool{
	"GROUP_NOT_EXIST": true, // grouping service: read or delete of a missing group
}

// IsNotFound reports whether err means the object does not exist: an HTTP 404, a
// RESTCONF `data-missing` error, an error whose structured Code is a known not-found
// code, or a list of per-entry results in which every failed entry is a 404.
// It does not search the message text.
func IsNotFound(err error) bool {
	ae := asAPIError(err)
	return ae != nil && (ae.StatusCode == http.StatusNotFound || ae.HasRestconfTag("data-missing") || notFoundCodes[ae.Code] || allEntries(ae.EntryStatus, http.StatusNotFound))
}

// allEntries reports whether the list is not empty and every code in it is want.
func allEntries(codes []int, want int) bool {
	for _, c := range codes {
		if c != want {
			return false
		}
	}
	return len(codes) > 0
}

// anyEntry reports whether the list holds the code want.
func anyEntry(codes []int, want int) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// IsConflict reports whether err is an HTTP 409, a RESTCONF `data-exists` error, or a list of
// per-entry results with a failed entry of 409.
func IsConflict(err error) bool {
	ae := asAPIError(err)
	return ae != nil && (ae.StatusCode == http.StatusConflict || ae.HasRestconfTag("data-exists") || anyEntry(ae.EntryStatus, http.StatusConflict))
}

// IsUnauthorized reports whether err is an HTTP 401 or 403.
func IsUnauthorized(err error) bool {
	ae := asAPIError(err)
	return ae != nil && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden)
}

// IsSessionLimit reports whether err is the login failure Crosswork returns when the
// user already has the maximum number of sessions: a 503 from the SSO ticket endpoint
// ("Per user session limit reached"). The text is not inspected. Release sessions with
// Logout, or raise NumParallelSessionsPerUser (GET/PUT /crosswork/aaa/v1/sessionconfig).
func IsSessionLimit(err error) bool {
	ae := asAPIError(err)
	return ae != nil && ae.StatusCode == http.StatusServiceUnavailable && strings.HasSuffix(ae.URL, ticketPath)
}

var errCode = regexp.MustCompile(`^\s*\[(Err[A-Za-z0-9_]+)\]`)

// codeFromMessage returns the [ErrXxx] code some DLM messages start with.
func codeFromMessage(msg string) string {
	if m := errCode.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

// natsMessage explains the gateway's "NATS request failed" 500: it means no handler
// could be reached for the path and method, or the body could not be parsed.
const natsMessage = "NATS request failed (no such route or unparseable request: check the path, the method and the JSON body)"

var springYAML = regexp.MustCompile(`(?m)^(message|error):\s*"?([^"\n]*)"?\s*$`)

// parseError extracts a message, a structured code and RESTCONF errors from an error body.
// It understands these shapes: RESTCONF errors (with or without the
// ietf-restconf namespace), the yang-patch status, CAS authentication exceptions,
// Spring error JSON, {"error":"..."}, {"code":N,"message":"..."}, errorMessage, errors[],
// and plain text.
func parseError(body string) (msg, code string, rc []RestconfError) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", "", nil
	}
	if !gjson.Valid(trimmed) || !gjson.Parse(trimmed).IsObject() {
		return plainMessage(trimmed), "", nil
	}
	r := gjson.Parse(trimmed)

	for _, p := range []string{"ietf-restconf:errors.error", "errors.error", "ietf-yang-patch:yang-patch-status.errors.error", "ietf-yang-patch:yang-patch-status.global-status.errors.error"} {
		rc = append(rc, restconfErrors(r.Get(p))...)
	}
	r.Get("ietf-yang-patch:yang-patch-status.edit-status.edit").ForEach(func(_, e gjson.Result) bool {
		rc = append(rc, restconfErrors(e.Get("errors.error"))...)
		return true
	})
	if len(rc) > 0 {
		var parts []string
		for _, e := range rc {
			if e.Message != "" {
				parts = append(parts, e.Message)
			} else {
				parts = append(parts, e.Tag)
			}
		}
		return strings.Join(parts, "; "), rc[0].Tag, rc
	}

	if ae := r.Get("authentication_exceptions"); ae.IsArray() {
		return strings.Join(stringList(ae), "; "), "", nil
	}
	for _, p := range []string{"message", "errorMessage"} {
		if v := r.Get(p); v.Type == gjson.String && v.String() != "" {
			return v.String(), codeFromMessage(v.String()), nil
		}
	}
	if v := r.Get("error"); v.Type == gjson.String && v.String() != "" {
		if v.String() == "NATS request failed" {
			return natsMessage, "", nil
		}
		return v.String(), codeFromMessage(v.String()), nil
	} else if v.IsObject() {
		for _, p := range []string{"message", "description", "code"} {
			if m := v.Get(p); m.Exists() && m.String() != "" {
				return m.String(), "", nil
			}
		}
	}
	if list := stringList(r.Get("errors")); len(list) > 0 {
		m := strings.Join(list, "; ")
		return m, codeFromMessage(list[0]), nil
	}
	return truncate(oneLine(trimmed), 300), "", nil
}

func restconfErrors(errs gjson.Result) []RestconfError {
	var out []RestconfError
	errs.ForEach(func(_, e gjson.Result) bool {
		out = append(out, RestconfError{
			Type:    e.Get("error-type").String(),
			Tag:     e.Get("error-tag").String(),
			AppTag:  e.Get("error-app-tag").String(),
			Path:    e.Get("error-path").String(),
			Message: e.Get("error-message").String(),
			Info:    e.Get("error-info").String(),
		})
		return true
	})
	return out
}

func stringList(arr gjson.Result) []string {
	var out []string
	if !arr.IsArray() {
		return nil
	}
	for _, v := range arr.Array() {
		if v.Type == gjson.String && v.String() != "" {
			out = append(out, v.String())
		}
	}
	return out
}

// plainMessage turns a non-JSON body into a short message. Spring's YAML-style error
// ("--- !<java.util.LinkedHashMap> ...") is reduced to its message or error line.
func plainMessage(s string) string {
	if strings.HasPrefix(s, "---") {
		var parts []string
		for _, m := range springYAML.FindAllStringSubmatch(s, -1) {
			parts = append(parts, strings.TrimSpace(m[2]))
		}
		if len(parts) > 0 {
			return strings.Join(parts, ": ")
		}
	}
	return truncate(oneLine(s), 300)
}
