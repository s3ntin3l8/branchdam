package probe

import (
	"bytes"
	"errors"
	"testing"

	_ "image/gif"
)

func TestDecodeBoundedRefusesHugeDeclaredDimensions(t *testing.T) {
	// GIF header declaring a 65535x65535 logical screen; DecodeConfig only
	// needs the header, so no pixel data follows.
	hdr := []byte("GIF89a\xff\xff\xff\xff\x00\x00\x00")
	_, _, err := DecodeBounded(bytes.NewReader(hdr))
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v, want ErrImageTooLarge", err)
	}
}
