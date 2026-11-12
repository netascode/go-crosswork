package crosswork

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The bodies in testdata/cwms_job*.json are Golden Configuration job results.

func TestParseApplyMessages(t *testing.T) {
	got := ParseApplyMessages(`[{"template":"t1","status":"failed","message":"Failed to apply"},{"template":"t2","status":"failed","message":"x"}]`)
	assert.Equal(t, []ApplyMessage{{"t1", "failed", "Failed to apply"}, {"t2", "failed", "x"}}, got)
	assert.Equal(t, []ApplyMessage{{Message: "Apply templates successful"}}, ParseApplyMessages("Apply templates successful"))
	assert.Equal(t, []ApplyMessage{{Message: "[not json"}}, ParseApplyMessages("[not json"))
	assert.Nil(t, ParseApplyMessages("  "))
}

// mopServer imitates the Automations status calls of one job. states are the job status answers in order, the last one repeats.
func mopServer(t *testing.T, states []string, resources string, details map[string]string) (func(http.ResponseWriter, *http.Request, int), *int32) {
	var polls int32
	return func(w http.ResponseWriter, r *http.Request, n int) {
		p := r.URL.Path
		switch {
		case p == mopStatusPath+"/70":
			i := int(atomic.AddInt32(&polls, 1)) - 1
			if i >= len(states) {
				i = len(states) - 1
			}
			job := gjson.Parse(fixture(t, "cwms_job_failed.json")).Value().(map[string]any)
			job["jobStatus"] = states[i]
			writeJSON(w, job)
		case p == mopStatusPath+"/70/resource":
			assert.Equal(t, "1", r.URL.Query().Get("page_number"), "the Automations lists count pages from 1")
			w.Write([]byte(resources))
		case strings.HasPrefix(p, mopStatusPath+"/70/resource/"):
			d, ok := details[strings.TrimPrefix(p, mopStatusPath+"/70/resource/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write([]byte(d))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}, &polls
}

func TestWaitMopJob(t *testing.T) {
	f := newFake(t)
	api, polls := mopServer(t, []string{"Pending", "Running", "Running", "Success"}, "{}", nil)
	f.api = api
	c := f.client(t)
	job, err := c.WaitMopJob(context.Background(), "70", time.Millisecond, 5*time.Second, NoLogPayload)
	require.NoError(t, err)
	assert.Equal(t, MopSuccess, job.Status)
	assert.Equal(t, int32(4), atomic.LoadInt32(polls))
	assert.True(t, job.Finished())

	f = newFake(t)
	api, _ = mopServer(t, []string{"Running"}, "{}", nil)
	f.api = api
	c = f.client(t)
	job, err = c.WaitMopJob(context.Background(), "70", 5*time.Millisecond, 40*time.Millisecond)
	require.ErrorIs(t, err, ErrMopJobWait)
	assert.Equal(t, MopRunning, job.Status, "the last status comes back with the error")

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err = c.WaitMopJob(ctx, "70", 5*time.Millisecond, 5*time.Second)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrMopJobWait)
}

func TestMopJobOutcome(t *testing.T) {
	// A job that ended Success while one device failed: the job status hides it, the outcome does not.
	resources := fixture(t, "cwms_job_resources.json")
	var out MopJobOutcome
	f := newFake(t)
	api, _ := mopServer(t, []string{"Success"}, resources, map[string]string{"86": fixture(t, "cwms_job_resource_failed.json"), "87": fixture(t, "cwms_job_resource_success.json")})
	f.api = api
	c := f.client(t)
	out, err := c.MopJobOutcome("70", NoLogPayload)
	require.NoError(t, err)
	assert.Equal(t, MopSuccess, out.Job.Status)
	assert.False(t, out.OK(), "a failed device makes the outcome not OK even when the job is Success")
	require.Len(t, out.Resources, 2)
	assert.Equal(t, "86", out.Resources[0].ID)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "cwtest-dev-gnmi", out.Failures[0].Device)
	require.Len(t, out.Failures[0].Messages, 1)
	assert.Equal(t, "cwtest-gc-d", out.Failures[0].Messages[0].Template)
	assert.Contains(t, out.Failures[0].String(), "Connection timed out")

	// All devices succeeded.
	ok := strings.Replace(resources, `"result":"Failed"`, `"result":"Success"`, 1)
	f = newFake(t)
	api, _ = mopServer(t, []string{"Success"}, ok, nil)
	f.api = api
	c = f.client(t)
	out, err = c.MopJobOutcome("70")
	require.NoError(t, err)
	assert.True(t, out.OK())
	assert.Empty(t, out.Failures)

	// A device whose detail cannot be read is still listed as a failure.
	f = newFake(t)
	api, _ = mopServer(t, []string{"Failed"}, resources, nil)
	f.api = api
	c = f.client(t)
	out, err = c.MopJobOutcome("70")
	require.NoError(t, err)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "cwtest-dev-gnmi", out.Failures[0].Device)
	assert.Equal(t, MopFailed, out.Failures[0].Result)
}

func TestMopJobResourcesPaged(t *testing.T) {
	var all []map[string]any
	for i := 0; i < 130; i++ {
		all = append(all, map[string]any{"name": fmt.Sprintf("d%d", i), "describeURI": fmt.Sprintf("http://x/v1/mop/status/job/70/resource/%d", i), "result": "Success", "deviceUuid": fmt.Sprintf("u%d", i)})
	}
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		page := 1
		fmt.Sscan(r.URL.Query().Get("page_number"), &page)
		size := 100
		start, end := (page-1)*size, page*size
		if end > len(all) {
			end = len(all)
		}
		b, _ := json.Marshal(map[string]any{"resources": all[start:end], "pagination": map[string]any{"total": len(all), "pageNumber": page, "pageSize": size}})
		w.Write(b)
	}
	c := f.client(t)
	got, err := c.MopJobResources("70")
	require.NoError(t, err)
	assert.Len(t, got, 130)
	assert.Equal(t, "129", got[129].ID)
}
