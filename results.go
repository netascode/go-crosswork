package crosswork

import (
	"log"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Failure detection for HTTP 2xx responses.
//
// Many Crosswork services answer 200 and put the failure in the body. A response is
// treated as a failure when its typed members match one of the shapes below, for
// example a job state or a status marker. A member that merely has an error-like name
// is not enough (counts such as {"error":11,"ok":22} are healthy). Anything that
// matches no rule is a success.

var jobFailureStates = map[string]bool{
	"JOB_FAILED":   true,
	"JOB_REJECTED": true,
	"JOB_INVALID":  true,
	"JOB_PARTIAL":  true,
}

// resultFailure reports whether a 2xx body describes a failure, with its message.
func resultFailure(res Res) (string, bool) {
	if res.IsArray() {
		msg, _, ok := entryFailure(res)
		return msg, ok
	}
	if !res.IsObject() {
		return "", false
	}

	// 1. Job object (DLM, NSO actions, Data Gateway): {"job_id":..., "state":"JOB_FAILED", "error":...}
	if res.Get("job_id").Type == gjson.String && res.Get("state").Type == gjson.String {
		state := res.Get("state").String()
		if state == "JOB_COMPLETED_WITH_WARNING" {
			log.Printf("[WARNING] Job completed with warning: %s", truncate(oneLine(res.Raw), 300))
		}
		if jobFailureStates[state] {
			if m := firstNonEmpty(res.Get("error").String(), strings.Join(stringList(res.Get("errors")), "; ")); m != "" {
				return m, true
			}
			return "job " + state, true
		}
		return "", false
	}

	// 2. Request result (collection, cross-cluster, AppManager, geo, placement): {"result":{"request_result":"REJECTED",...}}
	if rr := res.Get("result.request_result"); rr.Type == gjson.String && rr.String() == "REJECTED" {
		return firstNonEmpty(res.Get("result.error.error").String(), "request rejected"), true
	}

	// 3. Code and message (ZTP): {"code":400,"message":"Filter not provided"}
	if c, m := res.Get("code"), res.Get("message"); c.Type == gjson.Number && c.Float() >= 400 && m.Type == gjson.String {
		return m.String(), true
	}

	// 4. Fail marker (fault/alarm): {"error":"Fail","code":0,"message":"Input Request is invalid"}
	if e, m := res.Get("error"), res.Get("message"); e.Type == gjson.String && e.String() == "Fail" && m.Type == gjson.String && m.String() != "" {
		return m.String(), true
	}

	// 5. Lone error: the body has only `error` (non-empty string) or only `errors` (non-empty list of strings).
	keys := 0
	res.ForEach(func(_, _ gjson.Result) bool { keys++; return true })
	if keys == 1 {
		if e := res.Get("error"); e.Type == gjson.String && strings.TrimSpace(e.String()) != "" {
			return e.String(), true
		}
		if list := stringList(res.Get("errors")); len(list) > 0 && len(list) == len(res.Get("errors").Array()) {
			return strings.Join(list, "; "), true
		}
	}

	// 6. Status marker (grouping): {"status":"Error","error":"DUPLICATED_DEVICE_ACCESS_GROUP_NAME"}; the body can carry
	// the object next to it, so the rule looks at the marker, not at the number of members.
	if s := res.Get("status"); s.Type == gjson.String && s.String() == "Error" {
		return firstNonEmpty(res.Get("error").String(), "status Error"), true
	}
	return "", false
}

// entryFailure recognises a list of per-entry results, the answer of the Workflow Manager Solutions services
// (Golden Configuration) to a call that acts on several objects, with HTTP 207: each entry is
// {"code":409,"message":"...","status":"failed","key":"..."}. The list is a failure when any entry has a
// code of 400 or more, a message and the status "failed". It returns the messages of the failed entries
// (prefixed with their key when the list has several entries) and the codes of those entries. A list of
// anything else is not inspected.
func entryFailure(res Res) (string, []int, bool) {
	entries := res.Array()
	var msgs []string
	var codes []int
	for _, e := range entries {
		c, m, st := e.Get("code"), e.Get("message"), e.Get("status")
		if !e.IsObject() || c.Type != gjson.Number || c.Float() < 400 || m.Type != gjson.String || st.Type != gjson.String || !strings.EqualFold(st.String(), "failed") {
			continue
		}
		msg := m.String()
		if k := e.Get("key").String(); k != "" && len(entries) > 1 {
			msg = k + ": " + msg
		}
		msgs = append(msgs, msg)
		codes = append(codes, int(c.Int()))
	}
	if len(msgs) == 0 {
		return "", nil, false
	}
	return strings.Join(msgs, "; "), codes, true
}

// entryStatuses returns the codes of the failed entries of a list of per-entry results, nil for any other body.
func entryStatuses(res Res) []int {
	if !res.IsArray() {
		return nil
	}
	_, codes, _ := entryFailure(res)
	return codes
}

// statusErrorCode returns the structured error code of a status-marker failure (rule 6),
// such as GROUP_NOT_EXIST, and "" for any other body.
func statusErrorCode(res Res) string {
	if s := res.Get("status"); s.Type == gjson.String && s.String() == "Error" {
		if e := res.Get("error"); e.Type == gjson.String {
			return strings.TrimSpace(e.String())
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

var errorLikeKey = regexp.MustCompile(`(?i)error|fail|reject`)

// logUnmatched writes a debug line when a 2xx body has an error-like member with a
// non-empty string value but matches no rule, so an unrecognised failure shape shows in the debug log.
func logUnmatched(res Res) {
	if !res.IsObject() {
		return
	}
	res.ForEach(func(k, v gjson.Result) bool {
		if errorLikeKey.MatchString(k.String()) && v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
			log.Printf("[DEBUG] 2xx response has error-like member %q but matches no failure rule, treating it as success", k.String())
		}
		return true
	})
}
