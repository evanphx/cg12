package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
)

func main() {
	palette := color.Palette{
		color.RGBA{A: 255},
		color.RGBA{R: 255, A: 255},
		color.RGBA{G: 255, A: 255},
		color.RGBA{B: 255, A: 255},
	}
	bounds := image.Rect(0, 0, 5, 4)
	first := image.NewPaletted(bounds, palette)
	second := image.NewPaletted(bounds, palette)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			first.SetColorIndex(x, y, uint8((x+y)%len(palette)))
			second.SetColorIndex(x, y, uint8((2*x+y+1)%len(palette)))
		}
	}
	animation := &gif.GIF{
		Image:     []*image.Paletted{first, second},
		Delay:     []int{5, 7},
		LoopCount: 2,
		Disposal:  []byte{gif.DisposalNone, gif.DisposalBackground},
	}
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, animation); err != nil {
		panic("animated GIF encode failed: " + err.Error())
	}
	configuration, err := gif.DecodeConfig(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic("animated GIF config decode failed: " + err.Error())
	}
	if configuration.Width != bounds.Dx() || configuration.Height != bounds.Dy() {
		panic("GIF dimensions mismatch")
	}

	decoded, err := gif.DecodeAll(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		panic("animated GIF decode failed: " + err.Error())
	}
	if len(decoded.Image) != 2 || decoded.LoopCount != animation.LoopCount {
		panic("GIF animation metadata mismatch")
	}
	if decoded.Delay[0] != 5 || decoded.Delay[1] != 7 {
		panic("GIF frame delays mismatch")
	}
	if decoded.Disposal[0] != gif.DisposalNone || decoded.Disposal[1] != gif.DisposalBackground {
		panic("GIF disposal metadata mismatch")
	}
	if !bytes.Equal(decoded.Image[0].Pix, first.Pix) || !bytes.Equal(decoded.Image[1].Pix, second.Pix) {
		panic("GIF frame pixels mismatch")
	}
}
