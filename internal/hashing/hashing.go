// Package hashing implements branchDAM's dual fingerprinting model (spec
// Pillar 1): a cheap sampled fast_hash for instant path-remapping, and a
// full-stream full_hash for archive integrity verification. All functions
// are pure -- no filesystem access, no goroutines, no worker-pool awareness.
// internal/workers (PR 4) is what keeps these off the HTTP/watcher threads
// per spec directive 9.2; this package just computes.
package hashing

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"math/bits"

	"github.com/cespare/xxhash/v2"
	"github.com/corona10/goimagehash"
	"github.com/zeebo/blake3"
)

// fastHashSampleSize is 2MiB per region, matching spec Pillar 1.
const fastHashSampleSize = 2 * 1024 * 1024

// FastHash computes a cheap xxHash64 fingerprint over the first, middle, and
// last 2MiB of the file plus its total size, in that order. For files
// smaller than 6MiB the three regions overlap (even coincide entirely for a
// file under 2MiB) -- overlap is fine, since the goal is a stable per-file
// fingerprint for instant remap-on-move detection, not non-redundant
// sampling. It is NOT a substitute for FullHash: see docs/schema.md fix #8.
// Returns 16 lowercase hex characters (64 bits).
func FastHash(r io.ReaderAt, size int64) (string, error) {
	h := xxhash.New()
	buf := make([]byte, fastHashSampleSize)

	for _, reg := range sampleRegions(size) {
		n := reg.end - reg.start
		if n == 0 {
			continue
		}
		read, err := r.ReadAt(buf[:n], reg.start)
		if err != nil && (!errors.Is(err, io.EOF) || int64(read) != n) {
			return "", fmt.Errorf("fast hash: read region [%d,%d): %w", reg.start, reg.end, err)
		}
		if _, err := h.Write(buf[:read]); err != nil {
			return "", fmt.Errorf("fast hash: %w", err)
		}
	}

	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], uint64(size))
	if _, err := h.Write(sizeBuf[:]); err != nil {
		return "", fmt.Errorf("fast hash: %w", err)
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
}

type region struct{ start, end int64 }

// sampleRegions returns the (first, middle, last) 2MiB windows for size,
// clamped to [0, size] so they overlap rather than go out of bounds for
// small files. Deterministic for a given size -- callers never need to
// reason about which case applied.
func sampleRegions(size int64) [3]region {
	if size <= 0 {
		return [3]region{{0, 0}, {0, 0}, {0, 0}}
	}
	clamp := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		if v > size {
			return size
		}
		return v
	}

	first := region{0, clamp(fastHashSampleSize)}
	middleStart := clamp(size/2 - fastHashSampleSize/2)
	middle := region{middleStart, clamp(middleStart + fastHashSampleSize)}
	lastStart := clamp(size - fastHashSampleSize)
	last := region{lastStart, size}
	return [3]region{first, middle, last}
}

// FullHash computes a BLAKE3-256 digest of the entire stream. This is the
// column that backs "bit-for-bit archive verification before safe ejection"
// (spec §4) -- xxHash64 (fast_hash) is 64 bits and non-cryptographic, which
// docs/schema.md fix #8 explains is not sufficient for that claim. Returns
// 64 lowercase hex characters (256 bits).
func FullHash(r io.Reader) (string, error) {
	h := blake3.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("full hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PerceptualHash computes a 64-bit DCT perceptual hash of img (spec Pillar
// 1's Tier-3 heuristic matcher, wired up when internal/graph adds Tier-3
// resolvers). Takes an already-decoded image.Image rather than raw bytes or
// a file path -- decoding format selection belongs to the caller (the
// indexer/pipeline, which already knows the file's mime type), not here.
func PerceptualHash(img image.Image) (int64, error) {
	h, err := goimagehash.PerceptionHash(img)
	if err != nil {
		return 0, fmt.Errorf("perceptual hash: %w", err)
	}
	return int64(h.GetHash()), nil
}

// HammingDistance returns the number of differing bits between two
// perceptual hashes, e.g. for the spec's "pHash Hamming distance <= 10"
// Tier-3 matching threshold. Computed directly over the stored int64 rather
// than through goimagehash.ImageHash.Distance, which requires reconstructing
// a *ImageHash (and matching Kind) from a bare persisted value.
func HammingDistance(a, b int64) int {
	return bits.OnesCount64(uint64(a) ^ uint64(b))
}

// IsValidHex reports whether s contains exactly length hexadecimal characters (0-9, a-f, A-F).
func IsValidHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
