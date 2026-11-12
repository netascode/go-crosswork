package crosswork

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The write test creates the SR-TE ODN template cwtest-live-odn through the Crosswork NSO proxy,
// updates it, deletes it and creates it again at once, and removes it. It needs CW_WRITE=1.
// With CW_NSO_DEVICE set to a device of the NSO dispatch map the template has that head
// end, so the delete does real work on the device and the wait has something to wait for
// (without it NSO accepts the template and pushes nothing).
func TestLiveClientPlanWait_ODNTemplate(t *testing.T) {
	c := liveClient(t)
	if os.Getenv("CW_WRITE") != "1" {
		t.Skip("set CW_WRITE=1 to run the live test that creates and removes an ODN template")
	}
	const (
		obj  = "/crosswork/proxy/nso/restconf/data/cisco-sr-te-cfp:sr-te/cisco-sr-te-cfp-sr-odn:odn/odn-template=cwtest-live-odn"
		plan = "/crosswork/proxy/nso/restconf/data/cisco-sr-te-cfp:sr-te/cisco-sr-te-cfp-sr-odn:odn/odn-template-plan=cwtest-live-odn"
	)
	body := func(color int) string {
		b := Body{}.Set("name", "cwtest-live-odn").Set("color", color).SetRaw("dynamic", `{"pce":{},"metric-type":"igp"}`)
		if d := os.Getenv("CW_NSO_DEVICE"); d != "" {
			b = b.SetRaw("head-end", `[{"name":"`+d+`"}]`)
		}
		return Body{}.SetRaw("cisco-sr-te-cfp-sr-odn:odn-template", "["+b.Str+"]").Str
	}
	_, err := c.Get(obj, Query("content", "config"))
	if err != nil && StatusCode(err) == 403 && asAPIError(err).Code == "access-denied" {
		t.Skip("this account has no NSO access (RESTCONF access-denied)")
	}
	t.Cleanup(func() { _, _ = c.Delete(obj, PlanPath(plan)) })
	if err == nil {
		_, err = c.Delete(obj, PlanPath(plan)) // start clean
		require.NoError(t, err)
	}

	_, err = c.Put(obj, body(4990), PlanPath(plan))
	require.NoError(t, err)
	res, err := c.Get(plan, Query("content", "nonconfig"))
	require.NoError(t, err)
	assert.True(t, parsePlan(res).ready, "the plan is ready when the PUT returns")

	_, err = c.Put(obj, body(4991), PlanPath(plan))
	require.NoError(t, err, "an update waits too")

	_, err = c.Delete(obj, PlanPath(plan))
	require.NoError(t, err)
	_, err = c.Get(plan, Query("content", "nonconfig"))
	require.Error(t, err)
	assert.True(t, IsNotFound(err), "the plan is gone when the DELETE returns")

	// Without the wait this create is refused with "Service still in zombie state" while the head end is cleaned.
	_, err = c.Put(obj, body(4992), PlanPath(plan))
	require.NoError(t, err, "a create right after the delete works because the delete waited")
	_, err = c.Delete(obj, PlanPath(plan))
	require.NoError(t, err)

	_, err = c.Put(obj, body(0), PlanPath(plan))
	require.Error(t, err, "NSO refuses colour 0 (HTTP 400)")
	assert.Equal(t, 400, StatusCode(err))
	assert.False(t, IsNotFound(err))
}
