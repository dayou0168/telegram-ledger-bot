package bot

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestBuildAddressVerifyPNGLayout(t *testing.T) {
	raw, err := buildAddressVerifyPNG("TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ")
	if err != nil {
		t.Fatalf("buildAddressVerifyPNG error: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode png error: %v", err)
	}
	if got, want := img.Bounds().Dx(), addressVerifyWidth; got != want {
		t.Fatalf("width = %d, want %d", got, want)
	}
	if got, want := img.Bounds().Dy(), addressVerifyHeight; got != want {
		t.Fatalf("height = %d, want %d", got, want)
	}
	assertNearColor(t, img, image.Pt(10, 10), hexColor(0x03, 0xa7, 0x7b))
	assertNearColor(t, img, image.Pt(40, 240), hexColor(0xdb, 0x70, 0x00))
	assertNearColor(t, img, image.Pt(28, 232), color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
}

func assertNearColor(t *testing.T, img image.Image, pt image.Point, want color.RGBA) {
	t.Helper()
	r, g, b, a := img.At(pt.X, pt.Y).RGBA()
	got := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	const tolerance = 2
	if absInt(int(got.R)-int(want.R)) > tolerance ||
		absInt(int(got.G)-int(want.G)) > tolerance ||
		absInt(int(got.B)-int(want.B)) > tolerance ||
		absInt(int(got.A)-int(want.A)) > tolerance {
		t.Fatalf("pixel %v = %#v, want near %#v", pt, got, want)
	}
}
