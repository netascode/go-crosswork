package crosswork

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/h2non/gock.v1"
)

// gock replaces the client's Transport, so Insecure must not be applied after interception.
func TestClientDeleteBody_Gock(t *testing.T) {
	defer gock.Off()
	const host = "https://cnc.example.com:30603"

	gock.New(host).Post("/crosswork/sso/v1/tickets").Reply(201).BodyString("TGT-1-abc-cas")
	gock.New(host).Post("/crosswork/sso/v2/tickets/jwt").Reply(200).BodyString(newFakeJWT())
	gock.New(host).Delete("/crosswork/inventory/v1/credentials").
		MatchHeader("Content-Type", "application/json; charset=UTF-8").
		JSON(map[string]any{"data": []map[string]string{{"profile": "p1"}}}).
		Reply(200).
		JSON(map[string]any{"job_id": "j1", "state": "JOB_COMPLETED", "type": "1 credential(s) deleted successfully"})

	c, err := NewClient(host, "admin", "pw")
	require.NoError(t, err)
	gock.InterceptClient(c.HttpClient)

	res, err := c.DeleteBody("/crosswork/inventory/v1/credentials", `{"data":[{"profile":"p1"}]}`)
	require.NoError(t, err)
	assert.Equal(t, "JOB_COMPLETED", res.Get("state").String())
	assert.True(t, gock.IsDone(), "login and the DELETE with a body were all matched")
}

func newFakeJWT() string {
	f := &fakeCW{}
	return f.tokenFor(3600 * 1e9)
}
