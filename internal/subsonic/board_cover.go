package subsonic

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"sync"
	"time"
)

// 榜单没有专属图片时使用稳定的本地图。只生成一次，图片不含用户或歌曲信息。
// 保持为 PNG，让普通 Subsonic 图片缓存直接解码，不依赖 SVG 或外部封面插件。
var boardCoverPNG = sync.OnceValues(func() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{239, 246, 255, 255}), image.Point{}, draw.Src)
	for _, bar := range []struct {
		rect  image.Rectangle
		color color.RGBA
	}{
		{image.Rect(48, 140, 88, 200), color.RGBA{96, 165, 250, 255}},
		{image.Rect(108, 96, 148, 200), color.RGBA{59, 130, 246, 255}},
		{image.Rect(168, 52, 208, 200), color.RGBA{37, 99, 235, 255}},
	} {
		draw.Draw(img, bar.rect, image.NewUniform(bar.color), image.Point{}, draw.Src)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
})

func serveBoardCover(w http.ResponseWriter, r *http.Request) {
	data, err := boardCoverPNG()
	if err != nil {
		http.Error(w, "封面生成失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "board.png", time.Time{}, bytes.NewReader(data))
}
