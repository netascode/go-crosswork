package crosswork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The Automations API of the Workflow Manager Solutions services reports the outcome of a job started by one
// of them (Golden Configuration, Fleet Upgrade, ...). The call that starts a job answers at once with the job
// id; the status here says how it ended. The job status alone is not the verdict: a job whose failures stay
// inside its error budget ends Success while the per-device results show a Failed device, so MopJobOutcome
// reads both.

const (
	mopStatusPath = "/crosswork/cwms/automations/v1/mop/status/job"

	// Job and device states of the Automations API. A job or a device is finished in the first three.
	MopSuccess  = "Success"
	MopFailed   = "Failed"
	MopCanceled = "Canceled"
	MopPending  = "Pending"
	MopRunning  = "Running"
)

// ErrMopJobWait is the error (wrapped) returned by WaitMopJob when the job is still not finished at the timeout.
var ErrMopJobWait = errors.New("the job is not finished")

// MopJob is the status of a job.
type MopJob struct {
	ID string
	// Name is the job name.
	Name string
	// Status is one of the Mop constants.
	Status string
	// Message is the reason the job gives, for example that every device failed; empty on success.
	Message string
	// Raw is the whole answer. It echoes the job input, variable values included: never log it.
	Raw Res
}

// Finished reports whether the job has ended (Success, Failed or Canceled).
func (j MopJob) Finished() bool { return mopFinished(j.Status) }

func mopFinished(state string) bool {
	return state == MopSuccess || state == MopFailed || state == MopCanceled
}

// MopJobResource is the result of a job for one device.
type MopJobResource struct {
	// ID is the id of the resource, the last segment of its describeURI; it addresses MopJobResourceDetail.
	ID string
	// Name is the name of the device and DeviceUUID its uuid.
	Name       string
	DeviceUUID string
	// Result is one of the Mop constants.
	Result string
	Raw    Res
}

// Finished reports whether the device has ended (Success, Failed or Canceled).
func (r MopJobResource) Finished() bool { return mopFinished(r.Result) }

// ApplyMessage is one entry of the message of an Apply Config activity: the template and what NSO said.
type ApplyMessage struct {
	Template string
	Status   string
	Message  string
}

// MopJobFailure describes a device that did not succeed.
type MopJobFailure struct {
	Device     string
	DeviceUUID string
	// Result is the device result (Failed, Canceled, Pending or Running).
	Result string
	// Message is the message of the device and Messages the parsed messages of its activities.
	Message  string
	Messages []ApplyMessage
}

// String returns the failure on one line: the device, the result and the messages.
func (f MopJobFailure) String() string {
	var parts []string
	for _, m := range f.Messages {
		s := m.Message
		if m.Template != "" {
			s = m.Template + ": " + s
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 && f.Message != "" {
		parts = append(parts, f.Message)
	}
	return fmt.Sprintf("%s (%s): %s", f.Device, f.Result, strings.Join(parts, "; "))
}

// MopJobOutcome is the status of a job with the result of every device.
type MopJobOutcome struct {
	Job       MopJob
	Resources []MopJobResource
	// Failures holds every device that is not Success, with the detail the controller gives.
	Failures []MopJobFailure
}

// OK reports whether the job finished and every device succeeded.
func (o MopJobOutcome) OK() bool {
	if !o.Job.Finished() || len(o.Failures) > 0 || len(o.Resources) == 0 {
		return false
	}
	return o.Job.Status == MopSuccess
}

// MopJob returns the status of a job. Pass NoLogPayload: the answer echoes the job input.
func (client *Client) MopJob(id string, mods ...func(*Req)) (MopJob, error) {
	res, err := client.Get(mopStatusPath+"/"+id, mods...)
	if err != nil {
		return MopJob{}, err
	}
	return MopJob{ID: id, Name: res.Get("jobName").String(), Status: res.Get("jobStatus").String(),
		Message: res.Get("message").String(), Raw: res}, nil
}

// MopJobResources returns the result of a job for each device, following the pages.
func (client *Client) MopJobResources(id string, mods ...func(*Req)) ([]MopJobResource, error) {
	items, err := client.Paginate(&PageNumberPager{Path: mopStatusPath + "/" + id + "/resource", PageParam: "page_number",
		SizeParam: "page_size", FirstPage: 1, Size: 100, ItemsPath: "resources"}, mods...)
	out := make([]MopJobResource, 0, len(items))
	for _, it := range items {
		uri := it.Get("describeURI").String()
		out = append(out, MopJobResource{ID: uri[strings.LastIndex(uri, "/")+1:], Name: it.Get("name").String(),
			DeviceUUID: it.Get("deviceUuid").String(), Result: it.Get("result").String(), Raw: it})
	}
	return out, err
}

// MopJobResourceDetail returns the messages of one device of a job. The activities carry what NSO said.
func (client *Client) MopJobResourceDetail(jobID, resourceID string, mods ...func(*Req)) (MopJobFailure, error) {
	res, err := client.Get(mopStatusPath+"/"+jobID+"/resource/"+resourceID, mods...)
	if err != nil {
		return MopJobFailure{}, err
	}
	f := MopJobFailure{Device: res.Get("name").String(), DeviceUUID: res.Get("deviceUuid").String(),
		Result: res.Get("result").String(), Message: res.Get("message").String()}
	for _, a := range res.Get("activities").Array() {
		if a.Get("result").String() == MopSuccess {
			continue
		}
		f.Messages = append(f.Messages, ParseApplyMessages(a.Get("message").String())...)
	}
	return f, nil
}

// ParseApplyMessages reads the message of an Apply Config activity. On failure it is a JSON string that holds
// a list of {template, status, message}; any other text is returned as one message without a template.
func ParseApplyMessages(s string) []ApplyMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var list []struct {
		Template string `json:"template"`
		Status   string `json:"status"`
		Message  string `json:"message"`
	}
	if strings.HasPrefix(s, "[") && json.Unmarshal([]byte(s), &list) == nil && len(list) > 0 {
		out := make([]ApplyMessage, len(list))
		for i, m := range list {
			out[i] = ApplyMessage{Template: m.Template, Status: m.Status, Message: m.Message}
		}
		return out
	}
	return []ApplyMessage{{Message: s}}
}

// MopJobOutcome returns the status of a job with the result of every device and, for each device that is not
// Success, its detail. Call it when the job has finished (see WaitMopJob). Pass NoLogPayload.
func (client *Client) MopJobOutcome(id string, mods ...func(*Req)) (MopJobOutcome, error) {
	job, err := client.MopJob(id, mods...)
	if err != nil {
		return MopJobOutcome{}, err
	}
	out := MopJobOutcome{Job: job}
	out.Resources, err = client.MopJobResources(id, mods...)
	if err != nil {
		return out, err
	}
	for _, r := range out.Resources {
		if r.Result == MopSuccess {
			continue
		}
		f := MopJobFailure{Device: r.Name, DeviceUUID: r.DeviceUUID, Result: r.Result}
		if r.ID != "" {
			if d, err := client.MopJobResourceDetail(id, r.ID, mods...); err == nil {
				f = d
				f.Result = r.Result
			}
		}
		out.Failures = append(out.Failures, f)
	}
	return out, nil
}

// WaitForMopJob is a request option for a write that starts a MOP job (the answer carries the id in `key`): after the
// write, and while the lock of UseMutex or UseMutexKey is held, it polls the job every interval until it has finished or
// the timeout passes. It is an AfterWrite function, so a failure comes back as a *WaitError next to the response of the
// write; a job that is still running at the timeout wraps ErrMopJobWait, and an answer without a job id is an error too.
func (client *Client) WaitForMopJob(interval, timeout time.Duration) func(*Req) {
	return AfterWrite(func(ctx context.Context, res Res) error {
		id := res.Get("key").String()
		if id == "" {
			return errors.New("the answer of the write has no job id (key) to wait for")
		}
		_, err := client.WaitMopJob(ctx, id, interval, timeout, NoLogPayload)
		return err
	})
}

// WaitMopJob polls the status of a job every interval until it has finished or the timeout passes. On a timeout
// it returns the last status with an error that wraps ErrMopJobWait. A job still Pending, Running or Scheduled is not
// finished. The context cancels the wait. Pass NoLogPayload.
func (client *Client) WaitMopJob(ctx context.Context, id string, interval, timeout time.Duration, mods ...func(*Req)) (MopJob, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		job, err := client.MopJob(id, append([]func(*Req){Context(ctx)}, mods...)...)
		if err != nil {
			return job, err
		}
		if job.Finished() {
			return job, nil
		}
		wait := interval
		if left := time.Until(deadline); left < wait {
			wait = left
		}
		if wait <= 0 {
			return job, fmt.Errorf("%w after %s (status %s)", ErrMopJobWait, timeout, job.Status)
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(wait):
		}
	}
}
