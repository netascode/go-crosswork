package crosswork

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedact(t *testing.T) {
	in := `{"profile":"p","user_pass":[{"user_name":"u","password":"a\"b","enable_password_data":"x"}],"read_community":"public","token":"t","remote-password":"r","note":"keep"}`
	out := redact(in)
	for _, secret := range []string{`a\"b`, `"x"`, `public`, `"t"`, `"r"`} {
		assert.NotContains(t, out, secret)
	}
	assert.Contains(t, out, `"user_name":"u"`)
	assert.Contains(t, out, `"note":"keep"`)
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc...", truncate("abcdef", 3))
	assert.Equal(t, "abc", truncate("abc", 3))
}

func TestOneLine(t *testing.T) { assert.Equal(t, "a b c", oneLine("a\n  b\t c")) }

func TestCollapseSlashes(t *testing.T) { assert.Equal(t, "/a/b/c", collapseSlashes("//a///b/c")) }

func TestContains(t *testing.T) {
	assert.True(t, contains([]string{"a", "b"}, "b"))
	assert.False(t, contains([]string{"a"}, "z"))
}
