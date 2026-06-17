package harness

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// pngBytes encodes a w x h opaque PNG for the tests to feed to the decoder.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDownscaleCapsLongestEdge: a 4000x3000 image comes back with its longest
// edge clamped to maxEdge, aspect preserved, and still decodable. Breaker: drop
// the > maxEdge scale branch and the output keeps the original 4000px width.
func TestDownscaleCapsLongestEdge(t *testing.T) {
	out, mt, w, h, err := decodeAndDownscale(pngBytes(t, 4000, 3000))
	if err != nil {
		t.Fatal(err)
	}
	if w != maxEdge {
		t.Fatalf("longest edge must cap at %d, got width %d", maxEdge, w)
	}
	if h != 3000*maxEdge/4000 {
		t.Fatalf("aspect ratio not preserved: %dx%d", w, h)
	}
	if mt != "image/png" {
		t.Fatalf("media type: %q", mt)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("downscaled output must stay a decodable png: %v", err)
	}
	if cfg.Width != w || cfg.Height != h {
		t.Fatalf("encoded dims %dx%d disagree with reported %dx%d", cfg.Width, cfg.Height, w, h)
	}
}

// TestDownscaleLeavesSmallImage: an image already within the cap keeps its
// dimensions (no needless upscale or crop). Breaker: scale unconditionally and a
// small image's dimensions change.
func TestDownscaleLeavesSmallImage(t *testing.T) {
	_, _, w, h, err := decodeAndDownscale(pngBytes(t, 800, 600))
	if err != nil {
		t.Fatal(err)
	}
	if w != 800 || h != 600 {
		t.Fatalf("within-cap image must be left at its size, got %dx%d", w, h)
	}
}

// TestDecodePassThrough: bytes the stdlib cannot decode (here a WEBP header)
// come back unchanged with a detected media type and zero dimensions, instead of
// erroring. Breaker: return the decode error and an undecodable paste is lost.
func TestDecodePassThrough(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte("vp8 garbage")...)
	out, mt, w, h, err := decodeAndDownscale(webp)
	if err != nil {
		t.Fatalf("undecodable bytes must pass through, not error: %v", err)
	}
	if !bytes.Equal(out, webp) {
		t.Fatal("pass-through must return the original bytes unchanged")
	}
	if mt != "image/webp" {
		t.Fatalf("media type should be sniffed from the header, got %q", mt)
	}
	if w != 0 || h != 0 {
		t.Fatalf("undecodable image has unknown dimensions, got %dx%d", w, h)
	}
}

// TestEstimateImageTokens: the patch-grid estimate is ceil(w/28)*ceil(h/28),
// capped at maxEdge. Breaker: use floor division, or drop the cap, and one of
// these cases is wrong.
func TestEstimateImageTokens(t *testing.T) {
	// 56x56 = 2x2 patches exactly.
	if got := estimateImageTokens(56, 56); got != 4 {
		t.Fatalf("56x56 -> %d, want 4", got)
	}
	// 57x29 rounds up to 3x2 = 6 patches (proves ceiling, not floor).
	if got := estimateImageTokens(57, 29); got != 6 {
		t.Fatalf("57x29 -> %d, want 6 (ceil)", got)
	}
	// A large image is clamped to the cap.
	if got := estimateImageTokens(maxEdge, maxEdge); got != maxEdge {
		t.Fatalf("oversized estimate must clamp to %d, got %d", maxEdge, got)
	}
}

// TestDownscaleClampsTinyEdge: a long, thin image whose short edge would round to
// zero on downscale keeps at least one pixel and re-encodes to a real image.
// Breaker: drop the max(.,1) floor and the short edge is 0, yielding an empty image.
func TestDownscaleClampsTinyEdge(t *testing.T) {
	out, _, w, h, err := decodeAndDownscale(pngBytes(t, 2000, 1))
	if err != nil {
		t.Fatal(err)
	}
	if w != maxEdge || h != 1 {
		t.Fatalf("long-thin image must clamp the short edge to 1, got %dx%d", w, h)
	}
	if cfg, err := png.DecodeConfig(bytes.NewReader(out)); err != nil || cfg.Height != 1 {
		t.Fatalf("clamped output must be a real 1px-tall png: err=%v cfg=%+v", err, cfg)
	}
}
