package server

import (
	"image"
	"image/color"
	"math"
)

// Folder icons: a white sign on a coloured square, drawn here so that the
// bridge needs no image files. TVs show them beside the folders; an LG leaves
// out items without a picture.

const folderIconSize = 160 // the largest a DLNA thumbnail (PNG_TN) may be

// shape says whether a point of the unit square, (0,0) top left, is part of the
// sign.
type shape func(x, y float64) bool

type folderIcon struct {
	bg   color.RGBA
	sign shape
	fg   color.RGBA // white when zero
}

var folderIcons = map[string]folderIcon{
	idLive:          {bg: color.RGBA{0x24, 0x57, 0xd6, 0xff}, sign: tvSign},
	idFavorites:     {bg: color.RGBA{0xd9, 0x8c, 0x0b, 0xff}, sign: starSign},
	idRecent:        {bg: color.RGBA{0x1b, 0x7a, 0x3b, 0xff}, sign: clockSign},
	idRecordings:    {bg: color.RGBA{0x6a, 0x3d, 0xc4, 0xff}, sign: showsSign},
	idMovies:        {bg: color.RGBA{0x9b, 0x2c, 0x2c, 0xff}, sign: filmSign},
	idRecNow:        {bg: color.RGBA{0x30, 0x34, 0x3c, 0xff}, sign: recordSign, fg: color.RGBA{0xe5, 0x39, 0x35, 0xff}},
	idSources:       {bg: color.RGBA{0x0b, 0x7d, 0x8c, 0xff}, sign: towerSign},
	idListen:        {bg: color.RGBA{0xc2, 0x3b, 0x7a, 0xff}, sign: noteSign},
	idListenSources: {bg: color.RGBA{0x8a, 0x2b, 0x5a, 0xff}, sign: noteSign},
}

// folderIconImage draws a folder's icon, with each pixel's share of the sign
// sampled 4x4 times so that its edges are smooth.
func folderIconImage(id string, size int) (image.Image, bool) {
	fi, ok := folderIcons[id]
	if !ok {
		return nil, false
	}
	fg := fi.fg
	if fg == (color.RGBA{}) {
		fg = color.RGBA{0xff, 0xff, 0xff, 0xff}
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	const n = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			in := 0
			for sy := 0; sy < n; sy++ {
				for sx := 0; sx < n; sx++ {
					x := (float64(px) + (float64(sx)+.5)/n) / float64(size)
					y := (float64(py) + (float64(sy)+.5)/n) / float64(size)
					if fi.sign(x, y) {
						in++
					}
				}
			}
			a := float64(in) / (n * n)
			mix := func(b, f uint8) uint8 { return uint8(float64(b)*(1-a) + float64(f)*a + .5) }
			img.Set(px, py, color.RGBA{mix(fi.bg.R, fg.R), mix(fi.bg.G, fg.G), mix(fi.bg.B, fg.B), 0xff})
		}
	}
	return img, true
}

func inRect(x, y, x0, y0, x1, y1 float64) bool { return x >= x0 && x <= x1 && y >= y0 && y <= y1 }

// inFrame is the outline of a rectangle, w thick.
func inFrame(x, y, x0, y0, x1, y1, w float64) bool {
	return inRect(x, y, x0, y0, x1, y1) && !inRect(x, y, x0+w, y0+w, x1-w, y1-w)
}

func inCircle(x, y, cx, cy, r float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }

// inPolygon is the even-odd rule.
func inPolygon(x, y float64, pts [][2]float64) bool {
	in := false
	for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
		xi, yi, xj, yj := pts[i][0], pts[i][1], pts[j][0], pts[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// A screen on a stand.
func tvSign(x, y float64) bool {
	return inFrame(x, y, .2, .24, .8, .68, .06) || inRect(x, y, .46, .68, .54, .76) || inRect(x, y, .34, .76, .66, .81)
}

var starPoints = func() [][2]float64 {
	var p [][2]float64
	for i := 0; i < 10; i++ {
		r := .32
		if i%2 == 1 {
			r = .13
		}
		a := -math.Pi/2 + float64(i)*math.Pi/5
		p = append(p, [2]float64{.5 + r*math.Cos(a), .53 + r*math.Sin(a)})
	}
	return p
}()

func starSign(x, y float64) bool { return inPolygon(x, y, starPoints) }

// A clock at ten past.
func clockSign(x, y float64) bool {
	ring := inCircle(x, y, .5, .5, .3) && !inCircle(x, y, .5, .5, .24)
	return ring || inRect(x, y, .475, .3, .525, .525) || inRect(x, y, .475, .475, .66, .525)
}

// Two screens, one behind the other: many episodes.
func showsSign(x, y float64) bool {
	back := inFrame(x, y, .3, .22, .82, .6, .05) && !inRect(x, y, .18, .32, .72, .72)
	return back || inFrame(x, y, .18, .34, .7, .72, .05)
}

// A strip of film with its holes.
func filmSign(x, y float64) bool {
	if !inRect(x, y, .26, .18, .74, .82) {
		return false
	}
	for i := 0; i < 5; i++ {
		y0 := .22 + float64(i)*.12
		if inRect(x, y, .3, y0, .37, y0+.06) || inRect(x, y, .63, y0, .7, y0+.06) {
			return false // a hole
		}
	}
	return !inRect(x, y, .42, .24, .58, .48) && !inRect(x, y, .42, .52, .58, .76) // the frames
}

// A record dot inside a ring.
func recordSign(x, y float64) bool {
	return inCircle(x, y, .5, .5, .16) || (inCircle(x, y, .5, .5, .3) && !inCircle(x, y, .5, .5, .25))
}

// A pair of notes.
func noteSign(x, y float64) bool {
	heads := inCircle(x, y, .34, .7, .09) || inCircle(x, y, .64, .64, .09)
	stems := inRect(x, y, .405, .26, .45, .7) || inRect(x, y, .705, .2, .75, .64)
	beam := inPolygon(x, y, [][2]float64{{.405, .26}, {.75, .2}, {.75, .29}, {.405, .35}})
	return heads || stems || beam
}

// A mast sending.
func towerSign(x, y float64) bool {
	mast := inPolygon(x, y, [][2]float64{{.5, .42}, {.42, .8}, {.58, .8}}) || inCircle(x, y, .5, .38, .05)
	arcs := false
	for _, r := range []float64{.15, .25} {
		d := math.Hypot(x-.5, y-.38)
		if d >= r-.03 && d <= r+.03 && math.Abs(y-.38) < math.Abs(x-.5)*1.1 {
			arcs = true
		}
	}
	return mast || arcs
}
