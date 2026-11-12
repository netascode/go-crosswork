package crosswork

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	// the same words as go-restconf's rule, but a different error-tag: not retried
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
