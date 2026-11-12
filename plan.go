package crosswork

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// PlanStatusDetail is one entry of the `status-code-detail` list of an NSO plan: what the
// TSDN function pack reports for a component that failed.
type PlanStatusDetail struct {
	Type              string
	Name              string
	Code              string
	Severity          string
	Context           []string
	RecommendedAction string
	ImpactedDevice    string
}

// PlanError is returned when a write was accepted but its NSO plan did not reach the expected
// state: the plan failed, or it did not get there within the wait time.
//
// A failed plan after a DELETE means the object is already gone from the NSO configuration
// while its clean-up on the device failed; NSO keeps a zombie that must be recovered (the
// `tailf-ncs:zombies/service/re-deploy` action) before the same key can be created again.
type PlanError struct {
	// Path is the plan that was read.
	Path string
	// Failed is true when the plan reports a failure; Timeout is true when the wait time passed.
	Failed, Timeout bool
	// Message is the `error-info/message` of the plan, if any.
	Message string
	// Code is the status code of the first failed component, for example TSDN-SR-301.
	Code string
	// Details holds the `status-code-detail` entries.
	Details []PlanStatusDetail
	// State is a one-line summary of the plan at the end of the wait, for example
	// "self: init=not-reached ready=not-reached".
	State string
	// Raw is the body of the last plan read.
	Raw string
}

// Error implements the error interface.
func (e *PlanError) Error() string {
	var b strings.Builder
	switch {
	case e.Failed:
		b.WriteString("NSO plan failed")
	case e.Timeout:
		b.WriteString("timed out waiting for the NSO plan")
	default:
		b.WriteString("NSO plan error")
	}
	b.WriteString(" (" + e.Path + ")")
	if e.Code != "" {
		b.WriteString(": " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	for _, d := range e.Details {
		b.WriteString(fmt.Sprintf("; %s %s", d.Severity, d.Code))
		if d.ImpactedDevice != "" {
			b.WriteString(", device " + d.ImpactedDevice)
		}
		for _, c := range d.Context {
			b.WriteString(", " + c)
		}
		if d.RecommendedAction != "" {
			b.WriteString(", recommended action: " + d.RecommendedAction)
		}
	}
	if e.State != "" && !e.Failed {
		b.WriteString("; last state: " + e.State)
	}
	return b.String()
}

// IsPlanFailed reports whether err is a PlanError for a plan that failed.
func IsPlanFailed(err error) bool {
	var pe *PlanError
	return errors.As(err, &pe) && pe.Failed
}

// IsPlanTimeout reports whether err is a PlanError for a plan that did not finish in time.
func IsPlanTimeout(err error) bool {
	var pe *PlanError
	return errors.As(err, &pe) && pe.Timeout
}

// WaitForPlan reads the NSO plan at planPath (a full path, for example
// /crosswork/proxy/nso/restconf/data/cisco-sr-te-cfp:sr-te/cisco-sr-te-cfp-sr-odn:odn/odn-template-plan=x)
// every Client.PlanPollInterval until it is ready, or, with gone set, until it answers 404.
// A plan that reports a failure ends the wait with a *PlanError, and so does the end of
// maxSeconds (zero uses Client.DefaultMaxAsyncWaitTime).
//
// A write waits by itself when it carries the PlanPath option; WaitForPlan is for the callers
// that need to wait later. A plan that does not exist is "gone": for a write that is fine (the
// object has no plan), so the wait returns nil.
func (client *Client) WaitForPlan(planPath string, gone bool, maxSeconds int) error {
	if maxSeconds <= 0 {
		maxSeconds = client.DefaultMaxAsyncWaitTime
	}
	if maxSeconds <= 0 {
		maxSeconds = DefaultDefaultMaxAsyncWaitTime
	}
	interval := client.PlanPollInterval
	if interval <= 0 {
		interval = DefaultPlanPollInterval
	}
	return client.pollPlan(client.context(), planPath, gone, time.Duration(maxSeconds)*time.Second, interval)
}

// waitForPlan is the wait of a write that carries the PlanPath option.
func (client *Client) waitForPlan(req Req) error {
	max := req.MaxAsyncWaitTime
	if max <= 0 {
		max = client.DefaultMaxAsyncWaitTime
	}
	if max <= 0 {
		max = DefaultDefaultMaxAsyncWaitTime
	}
	interval := client.PlanPollInterval
	if interval <= 0 {
		interval = DefaultPlanPollInterval
	}
	return client.pollPlan(req.HttpReq.Context(), req.PlanPath, req.HttpReq.Method == http.MethodDelete,
		time.Duration(max)*time.Second, interval)
}

func (client *Client) pollPlan(ctx context.Context, planPath string, gone bool, max, interval time.Duration) error {
	deadline := time.Now().Add(max)
	var last *planState
	for {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, client.Url+planPath+"?content=nonconfig", nil)
		if err != nil {
			return err
		}
		res, err := client.Do(Req{HttpReq: r, LogPayload: true, noNSORetry: true, noResultCheck: true})
		if err != nil {
			if IsNotFound(err) {
				// No plan: a delete is finished, and a write needs no wait for an object without a plan.
				return nil
			}
			return err
		}
		st := parsePlan(res)
		last = &st
		if st.failed {
			return st.err(planPath, false)
		}
		if !gone && st.ready {
			return nil
		}
		if time.Now().Add(interval).After(deadline) {
			return st.err(planPath, true)
		}
		log.Printf("[DEBUG] Waiting for NSO plan %s: %s", planPath, st.summary)
		if !sleep(ctx, interval) {
			if last != nil {
				return last.err(planPath, true)
			}
			return ctx.Err()
		}
	}
}

// planState is what the wait needs from a plan.
type planState struct {
	failed  bool
	ready   bool
	summary string
	message string
	code    string
	details []PlanStatusDetail
	raw     string
}

func (st planState) err(path string, timeout bool) *PlanError {
	return &PlanError{Path: path, Failed: st.failed, Timeout: timeout && !st.failed, Message: st.message,
		Code: st.code, Details: st.details, State: st.summary, Raw: st.raw}
}

// parsePlan reads a plan read-back: {"<module>:<list>": [{"name": ..., "plan": {...}}]}.
// The plan failed when its root has the `failed` marker or a component state has the status
// `failed`; it is ready when the `self` component has reached its `ready` state.
func parsePlan(res Res) planState {
	st := planState{raw: res.Raw}
	var plan gjson.Result
	res.ForEach(func(_, v gjson.Result) bool {
		if v.IsArray() {
			v = v.Get("0")
		}
		plan = v.Get("plan")
		return false
	})
	if !plan.Exists() {
		st.summary = "no plan in the response"
		return st
	}
	if plan.Get("failed").Exists() {
		st.failed = true
	}
	var parts []string
	plan.Get("component").ForEach(func(_, c gjson.Result) bool {
		var states []string
		c.Get("state").ForEach(func(_, s gjson.Result) bool {
			name := s.Get("name").String()
			if i := strings.LastIndex(name, ":"); i >= 0 {
				name = name[i+1:]
			}
			status := s.Get("status").String()
			states = append(states, name+"="+status)
			if status == "failed" {
				st.failed = true
			}
			if c.Get("name").String() == "self" && name == "ready" && status == "reached" {
				st.ready = true
			}
			return true
		})
		parts = append(parts, c.Get("name").String()+": "+strings.Join(states, " "))
		if code := c.Get("status-code").String(); code != "" && st.code == "" {
			st.code = code
		}
		return true
	})
	st.summary = strings.Join(parts, "; ")
	st.message = plan.Get("error-info.message").String()
	plan.Get("status-code-detail").ForEach(func(_, d gjson.Result) bool {
		det := PlanStatusDetail{Type: d.Get("type").String(), Name: d.Get("name").String(), Code: d.Get("code").String(),
			Severity: d.Get("severity").String(), RecommendedAction: d.Get("recommended-action").String(),
			ImpactedDevice: d.Get("impacted-device").String()}
		d.Get("context").ForEach(func(_, c gjson.Result) bool {
			det.Context = append(det.Context, c.Get("context-name").String()+": "+c.Get("context-msg").String())
			return true
		})
		if st.code == "" {
			st.code = det.Code
		}
		st.details = append(st.details, det)
		return true
	})
	return st
}
