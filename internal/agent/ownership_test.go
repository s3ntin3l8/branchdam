package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidNodeUUID(t *testing.T) {
	assert.True(t, ValidNodeUUID("018f0000-0000-7000-8000-00000000000a"))
	assert.True(t, ValidNodeUUID("018F0000-0000-7000-8000-00000000000A"))
	for _, bad := range []string{"", "uuid-1", "../../etc/passwd", "018f0000-0000-7000-8000-00000000000a/../x", "018f0000000070008000-00000000000a", "018f0000-0000-7000-8000-00000000000g"} {
		assert.False(t, ValidNodeUUID(bad), bad)
	}
}

func TestFullHashUpdate(t *testing.T) {
	h1 := strings.Repeat("a", 64)
	h2 := strings.Repeat("b", 64)
	empty := ""

	apply, err := FullHashUpdate(nil, h1)
	assert.NoError(t, err)
	assert.True(t, apply, "no existing hash: set it")
	apply, err = FullHashUpdate(&empty, h1)
	assert.NoError(t, err)
	assert.True(t, apply)
	apply, err = FullHashUpdate(&h1, h1)
	assert.NoError(t, err)
	assert.False(t, apply, "identical: nothing to do")
	_, err = FullHashUpdate(&h1, h2)
	assert.ErrorIs(t, err, ErrHashConflict)
	_, err = FullHashUpdate(nil, "short")
	assert.ErrorIs(t, err, ErrMalformedPayload)
	_, err = FullHashUpdate(nil, strings.ToUpper(h1))
	assert.ErrorIs(t, err, ErrMalformedPayload, "uppercase is not the server's hash encoding")
}
