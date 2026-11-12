package crosswork

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseError(t *testing.T) {
	tests := []struct {
		file     string
		wantMsg  string
		wantCode string
		wantRC   int
	}{
		{"restconf_errors.json", "uri keypath not found", "invalid-value", 1},
		{"restconf_errors_plain.json", "is missing", "missing-attribute", 1},
		{"restconf_data_exists.json", "object already exists", "data-exists", 1},
		{"yang_patch_status_error.json", "bogus-leaf is not a valid element", "unknown-element", 1},
		{"cas_invalid_credentials.json", "Invalid credentials; Invalid credentials", "", 0},
		{"nats_error.json", "NATS request failed (no such route", "", 0},
		{"spring_error.json", "Required parameter 'dgID' is not present", "", 0},
		{"spring_error_no_message.json", "Not Found", "", 0},
		{"config_static_resource.json", "No static resource", "", 0},
		{"session_ended.json", "Your session has ended", "", 0},
		{"fault_fail.json", "Input Request is invalid", "", 0},
		{"ztp_code_message.json", "Filter not provided", "", 0},
		{"dlm_job_failed_code.json", "does not exist", "ErrNodeWithUuidDoesNotExist", 0},
		{"lone_errors.json", "criteria cannot be empty", "", 0},
		{"spring_yaml.txt", "Not Found", "", 0},
		{"plain_text.txt", "404 page not found", "", 0},
		{"no_credentials.txt", "No credentials can be extracted", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			msg, code, rc := parseError(fixture(t, tt.file))
			assert.Contains(t, msg, tt.wantMsg)
			assert.Equal(t, tt.wantCode, code)
			assert.Len(t, rc, tt.wantRC)
		})
	}
	msg, code, rc := parseError("  ")
	assert.Empty(t, msg)
	assert.Empty(t, code)
	assert.Empty(t, rc)
}
