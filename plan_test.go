package crosswork

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bodies in testdata/nso_*.json are NSO answers for the ODN template service.
const (
	odnPath  = nsoData + "/cisco-sr-te-cfp:sr-te/cisco-sr-te-cfp-sr-odn:odn/odn-template=x"
	odnPlan  = nsoData + "/cisco-sr-te-cfp:sr-te/cisco-sr-te-cfp-sr-odn:odn/odn-template-plan=x"
	fastPoll = 5 * time.Millisecond
)

// planAPI answers the write with writeStatus and then serves the plan: plans[i] on the i-th read
// (the last one repeats), where a plan is a fixture name or "404".
func planAPI(t *testing.T, writeStatus int, plans ...string) (func(http.ResponseWriter, *http.Request, int), *int32) {
	var reads int32
	return func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method != http.MethodGet {
			w.WriteHeader(writeStatus)
			return
		}
		i := int(atomic.AddInt32(&reads, 1)) - 1
		if i >= len(plans) {
			i = len(plans) - 1
		}
		if plans[i] == "404" {
			w.WriteHeader(404)
			fmt.Fprint(w, fixture(t, "nso_plan_gone_404.json"))
			return
		}
		fmt.Fprint(w, fixture(t, plans[i]))
	}, &reads
}

func TestPlanWait_PutWaitsForReady(t *testing.T) {
	f := newFake(t)
	api, reads := planAPI(t, 204, "nso_plan_removing.json", "nso_plan_removing.json", "nso_plan_ready.json")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Put(odnPath, `{}`, PlanPath(odnPlan))
	require.NoError(t, err)
	assert.EqualValues(t, 3, atomic.LoadInt32(reads), "the plan is read until self/ready is reached")
	assert.Equal(t, "nonconfig", f.last().Query[len("content="):], "the plan is read with content=nonconfig")
}

func TestPlanWait_DeleteWaitsForGone(t *testing.T) {
	f := newFake(t)
	api, reads := planAPI(t, 204, "nso_plan_removing.json", "nso_plan_removing.json", "404")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Delete(odnPath, PlanPath(odnPlan))
	require.NoError(t, err)
	assert.EqualValues(t, 3, atomic.LoadInt32(reads), "a delete waits for the plan to answer 404, a ready plan does not end it")
}

func TestPlanWait_DeleteDoesNotStopOnReady(t *testing.T) {
	f := newFake(t)
	api, reads := planAPI(t, 204, "nso_plan_ready.json", "404")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Delete(odnPath, PlanPath(odnPlan))
	require.NoError(t, err)
	assert.EqualValues(t, 2, atomic.LoadInt32(reads))
}

func TestPlanWait_FailedPlanIsPlanError(t *testing.T) {
	f := newFake(t)
	api, _ := planAPI(t, 204, "nso_plan_failed.json")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Delete(odnPath, PlanPath(odnPlan))
	require.Error(t, err)
	assert.True(t, IsPlanFailed(err))
	assert.False(t, IsPlanTimeout(err))
	assert.False(t, IsNotFound(err), "a failed plan is not a missing object")
	var pe *PlanError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "TSDN-SR-301", pe.Code)
	assert.Contains(t, pe.Message, "Failed to connect to device PE-1")
	require.Len(t, pe.Details, 1)
	assert.Equal(t, "PE-1", pe.Details[0].ImpactedDevice)
	assert.Equal(t, "ERROR", pe.Details[0].Severity)
	assert.Contains(t, pe.Details[0].Context[0], "Device unreachable")
	assert.Contains(t, err.Error(), "recommended action: Check device connectivity from NSO")
	assert.Contains(t, pe.Raw, "TSDN-SR-301")
}

func TestPlanWait_Timeout(t *testing.T) {
	f := newFake(t)
	api, reads := planAPI(t, 204, "nso_plan_removing.json")
	f.api = api
	c := f.client(t, PlanPollInterval(100*time.Millisecond))
	start := time.Now()
	_, err := c.Put(odnPath, `{}`, PlanPath(odnPlan), MaxAsyncWaitTime(1))
	require.Error(t, err)
	assert.True(t, IsPlanTimeout(err))
	assert.False(t, IsPlanFailed(err))
	assert.Contains(t, err.Error(), "last state: self: init=not-reached ready=not-reached")
	assert.GreaterOrEqual(t, atomic.LoadInt32(reads), int32(5))
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestPlanWait_ClientDefaultMaxWaitTime(t *testing.T) {
	f := newFake(t)
	api, _ := planAPI(t, 204, "nso_plan_removing.json")
	f.api = api
	c := f.client(t, PlanPollInterval(100*time.Millisecond), DefaultMaxAsyncWaitTime(1))
	_, err := c.Put(odnPath, `{}`, PlanPath(odnPlan))
	assert.True(t, IsPlanTimeout(err), "the client default applies when the request sets none")
	assert.Equal(t, 180, DefaultDefaultMaxAsyncWaitTime)
	d, _ := NewClient("https://x", "u", "p")
	assert.Equal(t, 180, d.DefaultMaxAsyncWaitTime)
	assert.Equal(t, time.Second, d.PlanPollInterval)
}

func TestPlanWait_NoWaitAndNoPlanPath(t *testing.T) {
	f := newFake(t)
	api, reads := planAPI(t, 204, "nso_plan_removing.json")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Put(odnPath, `{}`, PlanPath(odnPlan), NoWait)
	require.NoError(t, err)
	_, err = c.Put(odnPath, `{}`)
	require.NoError(t, err)
	assert.EqualValues(t, 0, atomic.LoadInt32(reads), "NoWait and a missing PlanPath do not read the plan")
}

func TestPlanWait_FailedWriteDoesNotWait(t *testing.T) {
	f := newFake(t)
	var reads int32
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&reads, 1)
		}
		w.WriteHeader(400)
		fmt.Fprint(w, fixture(t, "nso_zombie_400.json"))
	}
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Put(odnPath, `{}`, PlanPath(odnPlan))
	require.Error(t, err)
	assert.EqualValues(t, 0, atomic.LoadInt32(&reads))
}

func TestPlanWait_ObjectWithoutPlanIsNotAnError(t *testing.T) {
	f := newFake(t)
	api, reads := planAPI(t, 201, "404")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	_, err := c.Post(nsoData+"/x", `{}`, PlanPath(odnPlan))
	require.NoError(t, err)
	assert.EqualValues(t, 1, atomic.LoadInt32(reads))
}

func TestPlanWait_WaitForPlanWithoutAWrite(t *testing.T) {
	f := newFake(t)
	api, _ := planAPI(t, 204, "nso_plan_removing.json", "nso_plan_ready.json")
	f.api = api
	c := f.client(t, PlanPollInterval(fastPoll))
	require.NoError(t, c.WaitForPlan(odnPlan, false, 5))
}

func TestParsePlan_Shapes(t *testing.T) {
	for file, want := range map[string]struct{ failed, ready bool }{
		"nso_plan_ready.json":    {false, true},
		"nso_plan_removing.json": {false, false},
		"nso_plan_failed.json":   {true, false},
	} {
		st := parsePlan(Body{Str: fixture(t, file)}.Res())
		assert.Equal(t, want.failed, st.failed, file)
		assert.Equal(t, want.ready, st.ready, file)
	}
}

// The error values of NSO's answers to a write, which a caller has to tell apart.
func TestNSOWriteErrors(t *testing.T) {
	cases := []struct {
		file     string
		status   int
		contains string
		notFound bool
	}{
		{"nso_zombie_400.json", 400, "Service still in zombie state", false},
		{"nso_device_error_500.json", 500, "Unsupported behavior", false},
		{"nso_status_code_400.json", 400, "TSDN-IETF-L2NM-401", false},
		{"nso_interface_validation_400.json", 400, "TSDN-L2VPN-412", false},
		// NSO answers a write that could not reach the device with HTTP 404 and error-tag
		// invalid-value, the same shape as a missing object: IsNotFound is true, so the result of a
		// write must never be passed to IsNotFound.
		{"nso_device_unreachable_404.json", 404, "Failed to connect to device PE-1", true},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			f := newFake(t)
			f.api = func(w http.ResponseWriter, r *http.Request, n int) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, fixture(t, tc.file))
			}
			c := f.client(t)
			_, err := c.Put(odnPath, `{}`)
			require.Error(t, err)
			assert.Equal(t, tc.status, StatusCode(err))
			assert.Contains(t, err.Error(), tc.contains)
			assert.Equal(t, tc.notFound, IsNotFound(err))
			assert.Equal(t, 1, f.callCount(odnPath), "a refused write is not retried")
		})
	}
	assert.True(t, strings.Contains(fixture(t, "nso_device_error_500.json"), `\r\n`), "the device text keeps its line breaks")
}

func TestNSOWriteTimeout(t *testing.T) {
	get := func(method, path string) Req {
		c, _ := NewClient("https://x", "u", "p")
		return c.NewReq(method, path, nil)
	}
	assert.Equal(t, 120*time.Second, defaultTimeout(get("PUT", nsoData+"/x")))
	assert.Equal(t, 120*time.Second, defaultTimeout(get("DELETE", nsoData+"/x")))
	assert.Equal(t, 120*time.Second, defaultTimeout(get("PATCH", nsoData)))
	assert.Equal(t, time.Duration(0), defaultTimeout(get("GET", nsoData+"/x")))
	assert.Equal(t, time.Duration(0), defaultTimeout(get("PUT", "/crosswork/inventory/v1/nodes")))
}

func TestRequestTimeoutMayExceedClientTimeout(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(204)
	}
	c := f.client(t, RequestTimeout(100*time.Millisecond), MaxRetries(0))
	_, err := c.Put("/crosswork/inventory/v1/x", `{}`)
	require.Error(t, err, "the client timeout applies to a request without its own")
	_, err = c.Put("/crosswork/inventory/v1/x", `{}`, Timeout(2))
	require.NoError(t, err, "a per-request timeout replaces the client timeout, also when it is longer")
	_, err = c.Put(odnPath, `{}`)
	require.NoError(t, err, "a write under the NSO proxy gets 120 s without a request option")
}
