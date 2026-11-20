package crosswork

import (
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

const (
	nsoData  = "/crosswork/proxy/nso/restconf/data"
	nsoLocks = "/crosswork/proxy/nso/restconf/data/ietf-netconf-monitoring:netconf-state/datastores/datastore"
)

func TestClientYangPatch_Request(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprint(w, fixture(t, "yang_patch_status_ok.json"))
	}
	c := f.client(t)
	edits := []YangPatchEdit{
		NewYangPatchEdit("merge", "/tailf-ncs:devices/authgroups/group=g1", Body{}.SetRaw("tailf-ncs:group", `[{"name":"g1"}]`)),
		NewYangPatchEdit("remove", "/tailf-ncs:devices/authgroups/group=g2", Body{}),
	}
	res, err := c.YangPatch(nsoData, "patch-1", "two edits", edits)
	require.NoError(t, err)
	assert.Equal(t, "p1", res.Get(`ietf-yang-patch:yang-patch-status.patch-id`).String(), "the response is returned unchanged")

	got := f.last()
	assert.Equal(t, "PATCH", got.Method)
	assert.Equal(t, nsoData, got.Path)
	assert.Equal(t, "application/yang-patch+json", got.Header.Get("Content-Type"))
	assert.Equal(t, "application/yang-data+json", got.Header.Get("Accept"))
	assert.True(t, json.Valid([]byte(got.Body)))
	p := gjson.Parse(got.Body).Get(`ietf-yang-patch:yang-patch`)
	assert.Equal(t, "patch-1", p.Get("patch-id").String())
	assert.Equal(t, "two edits", p.Get("comment").String())
	assert.Equal(t, "0", p.Get("edit.0.edit-id").String())
	assert.Equal(t, "merge", p.Get("edit.0.operation").String())
	assert.Equal(t, "/tailf-ncs:devices/authgroups/group=g1", p.Get("edit.0.target").String())
	assert.Equal(t, "g1", p.Get(`edit.0.value.tailf-ncs:group.0.name`).String())
	assert.Equal(t, "1", p.Get("edit.1.edit-id").String())
	assert.False(t, p.Get("edit.1.value").Exists(), "a remove edit carries no value")
}

func TestClientYangPatch_FailureIsAPIError(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(400)
		fmt.Fprint(w, fixture(t, "yang_patch_status_error.json"))
	}
	c := f.client(t)
	res, err := c.YangPatch(nsoData, "p3", "", []YangPatchEdit{NewYangPatchEdit("merge", "/x", Body{Str: `{}`})})
	require.Error(t, err)
	assert.Equal(t, 400, StatusCode(err))
	assert.Contains(t, err.Error(), "bogus-leaf is not a valid element")
	assert.Equal(t, "unknown-element", asAPIError(err).Code)
	assert.True(t, res.Get(`ietf-yang-patch:yang-patch-status.edit-status`).Exists(), "Res keeps the body")
	assert.Equal(t, 1, f.callCount(nsoData), "a failed patch is not retried")
}

func TestNsoLocked(t *testing.T) {
	for file, want := range map[string]bool{
		"datastore_unlocked.json":       false,
		"datastore_locked_partial.json": true, // modelled on ietf-netconf-monitoring
		"datastore_locked_global.json":  true,
		"datastore_other_locked.json":   false, // only the candidate datastore is locked
		"nso_lock_denied.json":          false,
	} {
		assert.Equal(t, want, nsoLocked(gjson.Parse(fixture(t, file))), file)
	}
}

// nsoFake answers the patch with a rejection for the first `rejects` calls, then succeeds; the
// datastore reports a lock for the first `locked` polls.
func nsoFake(t *testing.T, rejectBody string, rejects, locked int32) (*fakeCW, *int32) {
	f := newFake(t)
	var patches, polls int32
	f.api = func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case nsoLocks:
			if atomic.AddInt32(&polls, 1) <= locked {
				fmt.Fprint(w, fixture(t, "datastore_locked_partial.json"))
			} else {
				fmt.Fprint(w, fixture(t, "datastore_unlocked.json"))
			}
		default:
			if atomic.AddInt32(&patches, 1) <= rejects {
				w.WriteHeader(409)
				fmt.Fprint(w, rejectBody)
				return
			}
			fmt.Fprint(w, fixture(t, "yang_patch_status_ok.json"))
		}
	}
	return f, &polls
}

func TestClientNSORetry_WaitsForLockThenRetries(t *testing.T) {
	for _, file := range []string{"nso_lock_denied.json", "nso_in_use.json"} {
		t.Run(file, func(t *testing.T) {
			f, polls := nsoFake(t, fixture(t, file), 1, 1)
			c := f.client(t, NSORetry(10))
			start := time.Now()
			_, err := c.Put(nsoData+"/x", `{}`) // a write; PUT, and the same holds for POST and PATCH
			require.NoError(t, err)
			assert.Equal(t, 2, f.callCount(nsoData+"/x"), "rejected once, then retried")
			assert.Equal(t, int32(2), atomic.LoadInt32(polls), "one poll saw the lock, the next saw it free")
			assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "it waited a second between polls")
		})
	}
}

func TestClientNSORetry_AppliesToEveryWriteMethod(t *testing.T) {
	for _, m := range []string{"POST", "PATCH", "DELETE"} {
		f, _ := nsoFake(t, fixture(t, "nso_lock_denied.json"), 1, 0)
		c := f.client(t, NSORetry(5))
		_, err := c.Do(c.NewReq(m, nsoData+"/x", nil))
		require.NoError(t, err, m)
		assert.Equal(t, 2, f.callCount(nsoData+"/x"), m+": lock-denied means nothing was applied, so retrying is safe")
	}
}

func TestClientNSORetry_OffByDefault(t *testing.T) {
	f, polls := nsoFake(t, fixture(t, "nso_lock_denied.json"), 1, 1)
	c := f.client(t)
	_, err := c.Put(nsoData+"/x", `{}`)
	require.Error(t, err)
	assert.True(t, IsConflict(err))
	assert.Equal(t, 1, f.callCount(nsoData+"/x"))
	assert.Equal(t, int32(0), atomic.LoadInt32(polls))
}

func TestClientNSORetry_IgnoresMessageTextAndOtherPaths(t *testing.T) {
	// lock wording in the message, but a different error-tag: not retried
	f, _ := nsoFake(t, fixture(t, "nso_text_only_lock.json"), 1, 0)
	c := f.client(t, NSORetry(5))
	_, err := c.Put(nsoData+"/x", `{}`)
	require.Error(t, err)
	assert.Equal(t, 1, f.callCount(nsoData+"/x"), "message text is never matched")

	// the right tag, but not under the NSO proxy
	f, _ = nsoFake(t, fixture(t, "nso_lock_denied.json"), 1, 0)
	c = f.client(t, NSORetry(5))
	other := "/crosswork/nbi/topology/v3/restconf/data/x"
	_, err = c.Put(other, `{}`)
	require.Error(t, err)
	assert.Equal(t, 1, f.callCount(other))

	// reads never take the lock
	f, _ = nsoFake(t, fixture(t, "nso_lock_denied.json"), 1, 0)
	c = f.client(t, NSORetry(5))
	_, err = c.Get(nsoData + "/x")
	require.Error(t, err)
	assert.Equal(t, 1, f.callCount(nsoData+"/x"))
}

func TestClientNSORetry_BoundedAndStopsOnCancel(t *testing.T) {
	// the lock never frees and NSO keeps refusing: bounded by MaxRetries and the lock timeout
	f, _ := nsoFake(t, fixture(t, "nso_lock_denied.json"), 99, 999)
	c := f.client(t, NSORetry(1), MaxRetries(2))
	start := time.Now()
	_, err := c.Put(nsoData+"/x", `{}`)
	require.Error(t, err)
	assert.True(t, IsConflict(err))
	assert.Equal(t, 3, f.callCount(nsoData+"/x"), "the first try plus MaxRetries retries")
	assert.Less(t, time.Since(start), 8*time.Second)

	// cancelling ends the wait at once
	f, _ = nsoFake(t, fixture(t, "nso_lock_denied.json"), 99, 999)
	c = f.client(t, NSORetry(60), MaxRetries(5))
	ctx, cancel := contextWithCancelAfter(200 * time.Millisecond)
	defer cancel()
	bound := c.WithContext(ctx)
	start = time.Now()
	_, err = bound.Put(nsoData+"/x", `{}`)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 4*time.Second, "a cancelled run does not sit out the 60 s lock wait")
}

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
	assert.Equal(t, 300, DefaultDefaultMaxAsyncWaitTime)
	d, _ := NewClient("https://x", "u", "p")
	assert.Equal(t, 300, d.DefaultMaxAsyncWaitTime)
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
