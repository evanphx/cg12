package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

func main() {
	bounds := image.Rect(0, 0, 5, 4)
	source := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			source.SetNRGBA(x, y, color.NRGBA{
				R: uint8(20*x + 3*y),
				G: uint8(7*x + 30*y),
				B: uint8(11*x + 13*y),
				A: uint8(100 + 7*x + 5*y),
			})
		}
	}

	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&encoded, source); err != nil {
		panic(err)
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic(err)
	}
	if configuration.Width != bounds.Dx() || configuration.Height != bounds.Dy() {
		panic("PNG dimensions mismatch")
	}

	decoded, err := png.Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic(err)
	}
	if decoded.Bounds() != bounds {
		panic("PNG bounds mismatch")
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			wantR, wantG, wantB, wantA := source.At(x, y).RGBA()
			gotR, gotG, gotB, gotA := decoded.At(x, y).RGBA()
			if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
				panic("PNG pixel mismatch")
			}
		}
	}
}
