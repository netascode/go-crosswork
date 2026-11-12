package crosswork

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveClientGet_ActiveTopology(t *testing.T) {
	c := liveClient(t)
	// a RESTCONF path: the library selects application/yang-data+json from the path
	res, err := c.Get("/crosswork/nbi/topology/v3/restconf/data/ietf-network-state:networks", Query("content", "nonconfig"))
	require.NoError(t, err)
	assert.True(t, res.IsObject())
	t.Logf("topology answered with %d bytes", len(res.Raw))
}

func TestLiveClientGet_NSOProxyNotFound(t *testing.T) {
	c := liveClient(t)
	_, err := c.Get("/crosswork/proxy/nso/restconf/data/tailf-ncs:devices/authgroups/group=cwtest-missing")
	require.Error(t, err)
	if StatusCode(err) == 403 && asAPIError(err).Code == "access-denied" {
		t.Skip("this account has no NSO access (RESTCONF access-denied)")
	}
	assert.True(t, IsNotFound(err), "a missing key is a 404 with a RESTCONF error: %v", err)
	assert.Equal(t, "invalid-value", asAPIError(err).Code)
}

// The write test creates NSO auth groups named cwtest-yp* through the Crosswork proxy and removes them.
func TestLiveClientYangPatch_NSOProxy(t *testing.T) {
	c := liveClient(t)
	if os.Getenv("CW_WRITE") != "1" {
		t.Skip("set CW_WRITE=1 to run the live test that creates and removes NSO auth groups")
	}
	const data = "/crosswork/proxy/nso/restconf/data"
	group := func(name string) string {
		return "/tailf-ncs:devices/authgroups/group=" + name
	}
	value := func(name string) Body {
		return Body{}.SetRaw("tailf-ncs:group", `[{"name":"`+name+`","default-map":{"remote-name":"cw","remote-password":"Cw-Live-1!"}}]`)
	}
	remove := func(names ...string) {
		var edits []YangPatchEdit
		for _, n := range names {
			edits = append(edits, NewYangPatchEdit("remove", group(n), Body{}))
		}
		_, _ = c.YangPatch(data, "cleanup", "", edits, NoLogPayload)
	}
	names := []string{"cwtest-yp1", "cwtest-yp2", "cwtest-yp3"}
	t.Cleanup(func() { remove(names...) })
	remove(names...) // start clean

	// two creates in one atomic call
	res, err := c.YangPatch(data, "live-1", "create two groups", []YangPatchEdit{
		NewYangPatchEdit("create", group("cwtest-yp1"), value("cwtest-yp1")),
		NewYangPatchEdit("create", group("cwtest-yp2"), value("cwtest-yp2")),
	}, NoLogPayload)
	require.NoError(t, err)
	assert.Equal(t, "live-1", res.Get(`ietf-yang-patch:yang-patch-status.patch-id`).String())
	for _, n := range names[:2] {
		_, err = c.Get(data + group(n))
		assert.NoError(t, err, n)
	}

	// atomic: a bad second edit leaves the first one unapplied, and the error says why
	_, err = c.YangPatch(data, "live-2", "", []YangPatchEdit{
		NewYangPatchEdit("create", group("cwtest-yp3"), value("cwtest-yp3")),
		NewYangPatchEdit("merge", group("cwtest-yp1"), Body{}.SetRaw("tailf-ncs:group", `[{"name":"cwtest-yp1","bogus-leaf":"x"}]`)),
	}, NoLogPayload)
	require.Error(t, err)
	assert.Equal(t, 400, StatusCode(err))
	assert.NotEmpty(t, asAPIError(err).RestconfErrors, "the edit-status errors are parsed")
	_, err = c.Get(data + group("cwtest-yp3"))
	assert.True(t, IsNotFound(err), "the first edit of a failed patch must not have been applied")

	// creating something that already exists is a conflict, not a text match
	_, err = c.Post(data+"/tailf-ncs:devices/authgroups", value("cwtest-yp1").Str, NoLogPayload)
	require.Error(t, err)
	assert.True(t, IsConflict(err), "%v", err)

	// remove both in one call
	_, err = c.YangPatch(data, "live-3", "", []YangPatchEdit{
		NewYangPatchEdit("remove", group("cwtest-yp1"), Body{}),
		NewYangPatchEdit("remove", group("cwtest-yp2"), Body{}),
	})
	require.NoError(t, err)
	_, err = c.Get(data + group("cwtest-yp1"))
	assert.True(t, IsNotFound(err))
}
