package crosswork

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

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
