package hashing

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestFastHashGoldenVectors pins FastHash's output for fixed inputs across
// the boundary cases its bespoke first/middle/last-2MiB sampling scheme
// actually has to handle: empty, well under 2MiB (all three regions
// coincide), exactly 2MiB (regions coincide at the boundary), and both
// sides of the "regions stop overlapping" threshold at 6MiB. There is no
// external reference implementation of this scheme to check against -- it's
// unique to this spec -- so these vectors are self-referential: they pin
// what the algorithm produces today and exist to catch an ACCIDENTAL change
// to the sampling/hashing logic, not to validate the scheme against a
// third-party source. Contrast with TestFullHashGoldenVectors below, which
// checks against the official BLAKE3 test vectors.
func TestFastHashGoldenVectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "34c96acdcadb1bbb"},
		{"tiny_13b", []byte("hello, world!"), "a17463135ce85764"},
		{"exactly_2mib", bytes.Repeat([]byte{0xAB}, 2*1024*1024), "c519d4df8b74ba0d"},
		{"3mib_pattern", patternBytes(3 * 1024 * 1024), "2c49f596a9f6d43e"},
		{"7mib_pattern", patternBytes(7 * 1024 * 1024), "18da4c4b0be74ade"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FastHash(bytes.NewReader(c.data), int64(len(c.data)))
			if err != nil {
				t.Fatalf("FastHash: %v", err)
			}
			if got != c.want {
				t.Errorf("FastHash(%s) = %q, want %q", c.name, got, c.want)
			}
			if len(got) != 16 {
				t.Errorf("len(FastHash(%s)) = %d, want 16 (matches the fast_hash length CHECK, docs/schema.md)", c.name, len(got))
			}
		})
	}
}

// TestFastHashDeterministicAndSensitive checks the two properties that
// actually matter for a fingerprint: same bytes -> same hash, and a change
// anywhere in the sampled regions (including just the trailing byte, which
// only the "last" region and the size-suffix could catch) -> a different
// hash.
func TestFastHashDeterministicAndSensitive(t *testing.T) {
	a := patternBytes(5 * 1024 * 1024)
	b := make([]byte, len(a))
	copy(b, a)

	h1, err := FastHash(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	h2, err := FastHash(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("identical content hashed differently: %q vs %q", h1, h2)
	}

	b[len(b)-1] ^= 0xFF // flip the very last byte
	h3, err := FastHash(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	if h1 == h3 {
		t.Error("flipping the last byte did not change FastHash -- the 'last 2MiB' region isn't being sampled")
	}
}

// TestFullHashGoldenVectors checks FullHash against the official BLAKE3
// test vectors (BLAKE3-team/BLAKE3, test_vectors/test_vectors.json),
// truncated to the 32-byte default output full_hash uses. This is what
// actually proves zeebo/blake3 is a conformant implementation, not just
// self-consistent.
func TestFullHashGoldenVectors(t *testing.T) {
	// The official vector generator fills input with the repeating sequence
	// 0, 1, 2, ..., 249, 250, 0, 1, ... -- input_len=1 is therefore a single
	// 0x00 byte.
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"input_len_0", nil, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"},
		{"input_len_1", []byte{0x00}, "2d3adedff11b61f14c886e35afa036736dcd87a74d27b5c1510225d0f592e213"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FullHash(bytes.NewReader(c.data))
			if err != nil {
				t.Fatalf("FullHash: %v", err)
			}
			if got != c.want {
				t.Errorf("FullHash(%s) = %q, want %q", c.name, got, c.want)
			}
			if len(got) != 64 {
				t.Errorf("len(FullHash(%s)) = %d, want 64 (matches the full_hash length CHECK, docs/schema.md)", c.name, len(got))
			}
		})
	}
}

func TestFullHashDeterministicAndSensitive(t *testing.T) {
	a := []byte("the quick brown fox jumps over the lazy dog")
	b := []byte("the quick brown fox jumps over the lazy dog.")

	h1, err := FullHash(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("FullHash: %v", err)
	}
	h2, err := FullHash(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("FullHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("identical content hashed differently: %q vs %q", h1, h2)
	}

	h3, err := FullHash(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("FullHash: %v", err)
	}
	if h1 == h3 {
		t.Error("a single appended byte did not change FullHash")
	}
}

func solidImage(w, h int, c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestPerceptualHashSameImageZeroDistance(t *testing.T) {
	img := solidImage(64, 64, color.RGBA{R: 120, G: 40, B: 200, A: 255})

	h1, err := PerceptualHash(img)
	if err != nil {
		t.Fatalf("PerceptualHash: %v", err)
	}
	h2, err := PerceptualHash(img)
	if err != nil {
		t.Fatalf("PerceptualHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hashing the same image twice gave different results: %d vs %d", h1, h2)
	}
	if d := HammingDistance(h1, h2); d != 0 {
		t.Errorf("HammingDistance(same image) = %d, want 0", d)
	}
}

// TestPerceptualHashDistinctImagesNonzeroDistance uses maximally different
// solid colors (black vs white) rather than asserting a specific distance
// value -- goimagehash's internal DCT coefficients are an implementation
// detail; the property this package's callers actually rely on (Tier-3
// resolvers, spec's Hamming distance <= 10 threshold) is that visually
// different images produce a measurably nonzero distance.
func TestPerceptualHashDistinctImagesNonzeroDistance(t *testing.T) {
	black := solidImage(64, 64, color.RGBA{A: 255})
	white := solidImage(64, 64, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	hBlack, err := PerceptualHash(black)
	if err != nil {
		t.Fatalf("PerceptualHash(black): %v", err)
	}
	hWhite, err := PerceptualHash(white)
	if err != nil {
		t.Fatalf("PerceptualHash(white): %v", err)
	}
	if d := HammingDistance(hBlack, hWhite); d == 0 {
		t.Error("HammingDistance(black, white) = 0, want > 0 for maximally different images")
	}
}

func TestHammingDistance(t *testing.T) {
	cases := []struct {
		a, b int64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, -1, 64}, // all 64 bits differ
		{0b1010, 0b0101, 4},
	}
	for _, c := range cases {
		if got := HammingDistance(c.a, c.b); got != c.want {
			t.Errorf("HammingDistance(%b, %b) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsValidHex(t *testing.T) {
	cases := []struct {
		input  string
		length int
		want   bool
	}{
		{"", 0, true},
		{"", 16, false},
		{"0123456789abcdef", 16, true},
		{"0123456789ABCDEF", 16, true},
		{"0123456789abcdef", 15, false},
		{"0123456789abcdef", 17, false},
		{"0123456789abcdeg", 16, false}, // 'g' is not hex
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", 64, true},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85z", 64, false},
	}
	for _, c := range cases {
		if got := IsValidHex(c.input, c.length); got != c.want {
			t.Errorf("IsValidHex(%q, %d) = %v, want %v", c.input, c.length, got, c.want)
		}
	}
}
