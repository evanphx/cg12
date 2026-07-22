package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

func main() {
	bounds := image.Rect(0, 0, 16, 12)
	source := image.NewRGBA(bounds)
	want := color.RGBA{R: 37, G: 101, B: 181, A: 255}
	draw.Draw(source, bounds, image.NewUniform(want), image.Point{}, draw.Src)

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 95}); err != nil {
		panic(err)
	}
	configuration, err := jpeg.DecodeConfig(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic(err)
	}
	if configuration.Width != bounds.Dx() || configuration.Height != bounds.Dy() {
		panic("JPEG dimensions mismatch")
	}

	decoded, err := jpeg.Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic(err)
	}
	if decoded.Bounds() != bounds {
		panic("JPEG bounds mismatch")
	}
	red, green, blue, alpha := decoded.At(8, 6).RGBA()
	if channelDifference(uint8(red>>8), want.R) > 4 ||
		channelDifference(uint8(green>>8), want.G) > 4 ||
		channelDifference(uint8(blue>>8), want.B) > 4 ||
		uint8(alpha>>8) != want.A {
		panic("JPEG decoded color outside tolerance")
	}
}

func channelDifference(left, right uint8) uint8 {
	if left > right {
		return left - right
	}
	return right - left
}
