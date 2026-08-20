package naming

import "testing"

// TestStem moved here from internal/pipeline/commit_test.go's TestFilenameStem
// (issue #132 criterion 3) when the stem/suffix-classification logic moved
// into this leaf package. Case-for-case identical to the pre-move table --
// Stem's output is unchanged by #132; only TestSuffixKind below is new.
func TestStem(t *testing.T) {
	cases := map[string]string{
		"DSC01234.ARW":        "dsc01234",
		"DSC01234_edited.jpg": "dsc01234_edited", // not a recognized suffix -- only the exact patterns below are stripped
		"render_v1_proxy.jpg": "render",
		"IMG_0001-2.jpg":      "img_0001",
		"IMG_0001 copy.jpg":   "img_0001",
		"IMG_0001(1).jpg":     "img_0001",
		"plain.jpg":           "plain",
		"no_extension_at_all": "no_extension_at_all",

		// H3 regression: a camera's own hyphen-numbered default naming
		// (Sony DSC-NNNN, some IMG-NNNN variants) must NOT collapse to a
		// bare "dsc"/"img" shared by every file in the shoot -- the -\d+
		// suffix branch is bounded to 1-2 digits precisely so these stay
		// distinct. See indexSuffixRe's doc comment.
		"DSC-0001.JPG": "dsc-0001",
		"IMG-1234.jpg": "img-1234",
		// The genuine "-N" duplicate-index case (OS auto-renaming, always
		// small) is still stripped -- 1-2 digits is the intended bound, not
		// a regression in disguise.
		"photo-2.jpg":  "photo",
		"photo-12.jpg": "photo",
		// 3+ digits after a hyphen reads as a camera serial/frame number,
		// not a duplicate index, and is deliberately left alone.
		"photo-123.jpg": "photo-123",

		// Issue #132: an unpadded 1-2 digit hyphen-numbering scheme -- a
		// camera counter that never reaches 3 digits, or a human numbering
		// a batch "trip-1.jpg".."trip-45.jpg" -- still reads as the "-N
		// duplicate index" pattern and collapses to a shared stem, same as
		// before. What's new is that Kind (TestSuffixKind below) now tells
		// internal/graph's FilenameStemResolver this collapse happened via
		// an index marker, so the resolver can gate on it (require a bare
		// anchor file, cap confidence below auto-accept) instead of trusting
		// the collapsed stem outright.
		"DSC-01.JPG": "dsc",
		"DSC-99.JPG": "dsc",
	}
	for in, want := range cases {
		if got := Stem(in); got != want {
			t.Errorf("Stem(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSuffixKind asserts Analyze's classification of the suffix it stripped
// (if any) -- the signal internal/graph's FilenameStemResolver gates
// AUTO_ACCEPTED on for issue #132.
func TestSuffixKind(t *testing.T) {
	cases := map[string]SuffixKind{
		// index markers -- duplicate-index suffixes
		"DSC-01.JPG":      SuffixIndex,
		"photo-2.jpg":     SuffixIndex,
		"photo-12.jpg":    SuffixIndex,
		"IMG_0001-2.jpg":  SuffixIndex,
		"IMG_0001(1).jpg": SuffixIndex,

		// role markers -- derivation-role suffixes
		"render_v1_proxy.jpg": SuffixRole,
		"IMG_0001 copy.jpg":   SuffixRole,

		// nothing stripped
		"DSC-0001.JPG":  SuffixNone,
		"plain.jpg":     SuffixNone,
		"photo-123.jpg": SuffixNone,

		// mixed: both an index and a role marker stripped -- index
		// ambiguity dominates, since that's the one the resolver gates on.
		"photo-2_edit.jpg": SuffixIndex,
	}
	for in, want := range cases {
		if got := Kind(in); got != want {
			t.Errorf("Kind(%q) = %v, want %v", in, got, want)
		}
	}
}
