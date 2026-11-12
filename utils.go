package crosswork

import (
	"io"
	"net/http"
	"regexp"
	"strings"
)

// readAll reads and closes a response body.
func readAll(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

// truncate shortens s to at most n bytes for logs and error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// oneLine collapses all whitespace, including newlines, into single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// secretField matches JSON members whose key looks like a secret, e.g. "password",
// "auth_password", "remote-password", "read_community", "token".
var secretField = regexp.MustCompile(`(?i)("[^"]*(?:password|passwd|secret|token|community|passphrase)[^"]*"\s*:\s*)"(?:[^"\\]|\\.)*"`)

// redact masks the values of secret-looking JSON members before a payload is logged.
func redact(s string) string {
	return secretField.ReplaceAllString(s, `$1"****"`)
}

var multiSlash = regexp.MustCompile(`/{2,}`)

// collapseSlashes turns any run of slashes into one, so documented paths such as
// /crosswork/optima-analytics//api/v1 can be matched.
func collapseSlashes(p string) string {
	return multiSlash.ReplaceAllString(p, "/")
}
