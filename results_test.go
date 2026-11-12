package crosswork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

// The first group of bodies must be recognised as failures; the second group looks like an error but is healthy.
func TestStatusErrorCode(t *testing.T) {
	assert.Equal(t, "DUPLICATED_DEVICE_ACCESS_GROUP_NAME", statusErrorCode(gjson.Parse(fixture(t, "grouping_error.json"))))
	assert.Equal(t, "DEVICE_ACCESS_GROUP_USER_ASSOCIATION_FAILURE", statusErrorCode(gjson.Parse(fixture(t, "grouping_error_with_group.json"))))
	assert.Equal(t, "", statusErrorCode(gjson.Parse(fixture(t, "grouping_success.json"))))
	assert.Equal(t, "", statusErrorCode(gjson.Parse(fixture(t, "lone_error.json"))))
}

func TestResultFailure(t *testing.T) {
	failures := map[string]string{
		"dlm_job_failed.json":             "already exists",
		"dlm_job_failed_code.json":        "does not exist",
		"dlm_job_failed_errors_list.json": "Invalid credential list",
		"dlm_job_partial.json":            "one device could not be deleted",
		"collection_rejected.json":        "filter must not be empty",
		"ztp_code_message.json":           "Filter not provided",
		"fault_fail.json":                 "Input Request is invalid",
		"lone_error.json":                 "Geo Mode is not enabled",
		"lone_errors.json":                "criteria cannot be empty",
		"dg_mapping_job_failed.json":      "requested dg is not the same as current one",
		"grouping_error.json":             "DUPLICATED_DEVICE_ACCESS_GROUP_NAME",
		"grouping_error_with_group.json":  "DEVICE_ACCESS_GROUP_USER_ASSOCIATION_FAILURE",
	}
	for name, want := range failures {
		t.Run("failure/"+name, func(t *testing.T) {
			msg, ok := resultFailure(gjson.Parse(fixture(t, name)))
			assert.True(t, ok, "must be recognised as a failure")
			assert.Contains(t, msg, want)
		})
	}

	healthy := []string{
		"dlm_job_completed.json",
		"dlm_job_completed_warning.json",
		"dg_mapping_job_completed.json",
		"dg_mapping_job_warning.json", // a device added admin up without a gateway: completed, moved down
		"dg_pool_created.json",
		"dlm_job_running.json", // not terminal, but not an error: the caller may poll
		"collection_accepted.json",
		"operstatesummary.json", // "error" is a count
		"nodes_query_with_errors.json",
		"collection_count.json",
		"password_policy.json",
		"ztp_count.json",
		"empty_error_fields.json",
		"code_message_ok.json",
		"scalar_zero.json",
		"list_body.json",
		"devicepackage_not_found.json", // soft not-found with no error member: left to the caller
		"yang_patch_status_ok.json",
		"grouping_success.json", // status marker is Success
	}
	for _, name := range healthy {
		t.Run("healthy/"+name, func(t *testing.T) {
			msg, ok := resultFailure(gjson.Parse(fixture(t, name)))
			assert.False(t, ok, "must not be treated as a failure, got %q", msg)
		})
	}
}

func TestResultFailure_NeedsTypedMembers(t *testing.T) {
	for name, body := range map[string]string{
		"state without job_id":      `{"state":"JOB_FAILED"}`,
		"job_id with numeric state": `{"job_id":"x","state":7}`,
		"request_result lowercase":  `{"result":{"request_result":"rejected"}}`,
		"code below 400":            `{"code":399,"message":"x"}`,
		"code as string":            `{"code":"400","message":"x"}`,
		"fail marker without msg":   `{"error":"Fail","code":0}`,
		"error with data":           `{"error":"x","data":[1]}`,
		"errors with empty entry":   `{"errors":["a",""]}`,
		"empty error string":        `{"error":"  "}`,
		"not an object":             `["error"]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := resultFailure(gjson.Parse(body))
			assert.False(t, ok)
		})
	}
}

func TestResultFailure_JobMessage(t *testing.T) {
	msg, ok := resultFailure(gjson.Parse(`{"job_id":"a","state":"JOB_REJECTED"}`))
	assert.True(t, ok)
	assert.Equal(t, "job JOB_REJECTED", msg, "a failed job with no text still reports its state")
}

func TestClientDo_FailureInside200(t *testing.T) {
	f := newFake(t)
	f.api = body(t, "dlm_job_failed.json")
	c := f.client(t)
	res, err := c.Post("/crosswork/inventory/v1/credentials", `{"data":[]}`)
	require.Error(t, err)
	var ae *APIError
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, 200, ae.StatusCode)
	assert.Contains(t, ae.Message, "already exists")
	assert.Contains(t, err.Error(), "StatusCode 200")
	assert.Equal(t, "JOB_FAILED", res.Get("state").String(), "Res still holds the parsed body next to the error")
	assert.False(t, IsConflict(err), "the library never reads prose: the caller reads first")
	assert.Equal(t, 1, f.callCount("/crosswork/inventory/v1/credentials"), "a failed write is not retried")
}

func TestClientDo_HealthyBodiesPass(t *testing.T) {
	for _, name := range []string{"operstatesummary.json", "dlm_job_completed.json", "nodes_query_with_errors.json", "scalar_zero.json", "devicepackage_not_found.json"} {
		f := newFake(t)
		f.api = body(t, name)
		c := f.client(t)
		res, err := c.Get(apiPath)
		require.NoError(t, err, name)
		assert.True(t, res.Exists(), name)
	}
}

func TestClientDo_ResultCheckOverrides(t *testing.T) {
	f := newFake(t)
	f.api = body(t, "dlm_job_failed.json")
	c := f.client(t)

	_, err := c.Post(apiPath, `{}`, NoResultCheck)
	assert.NoError(t, err, "NoResultCheck disables the built-in rules")

	_, err = c.Post(apiPath, `{}`, ResultCheck(func(r Res) error { return nil }))
	assert.NoError(t, err, "a custom check replaces the built-in rules")

	_, err = c.Post(apiPath, `{}`, ResultCheck(func(r Res) error { return fmt.Errorf("custom: %s", r.Get("state").String()) }))
	require.Error(t, err)
	assert.Equal(t, 200, StatusCode(err))
	assert.Contains(t, err.Error(), "custom: JOB_FAILED")
}

func TestEntryFailure(t *testing.T) {
	msg, ok := resultFailure(gjson.Parse(fixture(t, "cwms_entries_failed.json")))
	assert.True(t, ok)
	assert.Contains(t, msg, "associated with devices")
	assert.NotContains(t, msg, "42:", "a single entry is not prefixed with its key")

	msg, ok = resultFailure(gjson.Parse(fixture(t, "cwms_entries_mixed.json")))
	assert.True(t, ok)
	assert.Equal(t, "b: Application not found; c: in use", msg, "the failed entries only, each with its key")
	assert.Equal(t, []int{404, 409}, entryStatuses(gjson.Parse(fixture(t, "cwms_entries_mixed.json"))))

	_, ok = resultFailure(gjson.Parse(fixture(t, "cwms_entries_ok.json")))
	assert.False(t, ok, "a list of successful entries is a success")
	msg, ok = resultFailure(gjson.Parse(fixture(t, "cwms_single_failed.json")))
	assert.True(t, ok, "a single failed entry is the ZTP shape")
	assert.Contains(t, msg, "Vendor 'Cisco'")

	for _, body := range []string{`[]`, `[1,2]`, `[{"code":500,"message":"x"}]`, `[{"code":200,"message":"x","status":"failed"}]`, `[{"name":"a","error":"x"}]`} {
		_, ok = resultFailure(gjson.Parse(body))
		assert.False(t, ok, body)
	}
}
