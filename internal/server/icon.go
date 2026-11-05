package server

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
)

// icon draws a white play triangle on a blue square: the server's icon, which
// TVs show beside its name.
func icon(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg, fg := color.RGBA{0x24, 0x57, 0xd6, 0xff}, color.RGBA{0xff, 0xff, 0xff, 0xff}
	f := float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, bg)
			// A triangle with its corners at (.35,.25), (.35,.75) and (.75,.5).
			fx, fy := (float64(x)+.5)/f, (float64(y)+.5)/f
			if fx >= .35 && fx <= .75 {
				half := (.75 - fx) / .4 * .25
				if fy >= .5-half && fy <= .5+half {
					img.Set(x, y, fg)
				}
			}
		}
	}
	return img
}

var iconSizes = []int{48, 120}

// iconFile makes the bytes of /icons/logo<size>.png or .jpg, or of a top
// folder's icon, /icons/folder-<id>.png.
func iconFile(name string) ([]byte, string, bool) {
	if id, ok := strings.CutSuffix(strings.TrimPrefix(name, "folder-"), ".png"); ok && strings.HasPrefix(name, "folder-") {
		img, ok := folderIconImage(id, folderIconSize)
		if !ok {
			return nil, "", false
		}
		var b bytes.Buffer
		png.Encode(&b, img)
		return b.Bytes(), "image/png", true
	}
	for _, size := range iconSizes {
		var b bytes.Buffer
		switch name {
		case "logo" + strconv.Itoa(size) + ".png":
			png.Encode(&b, icon(size))
			return b.Bytes(), "image/png", true
		case "logo" + strconv.Itoa(size) + ".jpg":
			jpeg.Encode(&b, icon(size), &jpeg.Options{Quality: 90})
			return b.Bytes(), "image/jpeg", true
		}
	}
	return nil, "", false
}
