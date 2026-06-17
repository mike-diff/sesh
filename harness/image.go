// Image ingest: decode a pasted PNG or JPEG, downscale it for token hygiene,
// and re-encode it, using only the standard library so the single binary keeps
// its zero-dependency promise. Formats the stdlib cannot decode (for example
// WEBP) pass through untouched, carrying their detected media type and unknown
// dimensions. The token estimate is the patch-grid heuristic vision models use.
package harness

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
)

// maxEdge is the longest-edge ceiling images are downscaled to: the size above
// which a vision model gains no detail but the request keeps paying tokens.
const maxEdge = 1568

// decodeAndDownscale decodes raw image bytes, scales them so the longest edge is
// at most maxEdge, and re-encodes in the source format. It returns the encoded
// bytes, their media type, and the (possibly reduced) dimensions. An image that
// is already within the cap is re-encoded unchanged. Bytes the stdlib cannot
// decode pass through verbatim with a detected media type and zero dimensions,
// so an undecodable paste still has a media type to send.
func decodeAndDownscale(raw []byte) (out []byte, mediaType string, w, h int, err error) {
	src, format, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		return raw, detectMediaType(raw), 0, 0, nil
	}
	mediaType = "image/" + format
	if format == "jpeg" {
		mediaType = "image/jpeg"
	}

	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := src
	dw, dh := sw, sh
	if longest := max(sw, sh); longest > maxEdge {
		dw = sw * maxEdge / longest
		dh = sh * maxEdge / longest
		// A very long, thin image rounds the short edge to zero; keep at least
		// one pixel so the re-encode produces a real image, not an empty one.
		dw, dh = max(dw, 1), max(dh, 1)
		dst = scale(src, dw, dh)
	}

	encoded, eerr := encode(dst, format)
	if eerr != nil {
		return nil, "", 0, 0, eerr
	}
	return encoded, mediaType, dw, dh, nil
}

// scale resizes src to dw x dh by nearest-neighbor sampling. It is a box filter
// without the smoothing a resampling filter would add, which is acceptable for
// shrinking screenshots and keeps the dependency footprint at zero.
func scale(src image.Image, dw, dh int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*sh/dh
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*sw/dw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// encode writes img back to its source format. JPEG keeps a high quality so a
// re-encode of an already-lossy paste does not visibly degrade it.
func encode(img image.Image, format string) ([]byte, error) {
	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// detectMediaType sniffs the media type for bytes the stdlib could not decode,
// so a pass-through image still carries a type for the wire call.
func detectMediaType(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(raw, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(raw) >= 12 && bytes.Equal(raw[0:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// estimateImageTokens approximates an image's input-token cost from its pixel
// dimensions: one token per 28x28 patch, capped at maxEdge. It is for display,
// not billing.
func estimateImageTokens(w, h int) int {
	tokens := ceilDiv(w, 28) * ceilDiv(h, 28)
	if tokens > maxEdge {
		return maxEdge
	}
	return tokens
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }
