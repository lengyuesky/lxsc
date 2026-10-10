package subsonic

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"
)

// 本地默认封面不含歌曲或用户信息。仅生成一次，不占用任何上游并发名额。
var placeholderCoverPNG = sync.OnceValues(func() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{241, 245, 249, 255}), image.Point{}, draw.Src)
	ink := color.RGBA{100, 116, 139, 255}
	for _, rect := range []image.Rectangle{
		image.Rect(98, 64, 114, 174), image.Rect(174, 52, 190, 160), image.Rect(100, 52, 190, 76),
	} {
		draw.Draw(img, rect, image.NewUniform(ink), image.Point{}, draw.Src)
	}
	for _, center := range []image.Point{{84, 178}, {160, 164}} {
		for y := -22; y <= 22; y++ {
			for x := -30; x <= 30; x++ {
				if x*x*22*22+y*y*30*30 <= 30*30*22*22 {
					img.SetRGBA(center.X+x, center.Y+y, ink)
				}
			}
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
})

func placeholderCoverImage() (coverImage, error) {
	data, err := placeholderCoverPNG()
	return coverImage{data: data, contentType: "image/png", origin: "placeholder"}, err
}
