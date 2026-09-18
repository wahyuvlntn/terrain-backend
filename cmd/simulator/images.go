package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Simple procedural illustrations for locations and equipment.

type canvas struct{ img *image.RGBA }

func newCanvas(w, h int) *canvas { return &canvas{img: image.NewRGBA(image.Rect(0, 0, w, h))} }

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 255} }

func mix(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 255}
}

func (c *canvas) gradient(top, bottom color.RGBA) {
	b := c.img.Bounds()
	for y := 0; y < b.Dy(); y++ {
		col := mix(top, bottom, float64(y)/float64(b.Dy()))
		for x := 0; x < b.Dx(); x++ {
			c.img.SetRGBA(x, y, col)
		}
	}
}

func (c *canvas) rect(x0, y0, x1, y1 int, col color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if image.Pt(x, y).In(c.img.Bounds()) {
				c.img.SetRGBA(x, y, col)
			}
		}
	}
}

func (c *canvas) frame(x0, y0, x1, y1, w int, col color.RGBA) {
	c.rect(x0, y0, x1, y0+w, col)
	c.rect(x0, y1-w, x1, y1, col)
	c.rect(x0, y0, x0+w, y1, col)
	c.rect(x1-w, y0, x1, y1, col)
}

func (c *canvas) circle(cx, cy, r int, col color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r && image.Pt(x, y).In(c.img.Bounds()) {
				c.img.SetRGBA(x, y, col)
			}
		}
	}
}

func (c *canvas) ring(cx, cy, r, w int, col color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			d := math.Hypot(float64(x-cx), float64(y-cy))
			if d <= float64(r) && d >= float64(r-w) && image.Pt(x, y).In(c.img.Bounds()) {
				c.img.SetRGBA(x, y, col)
			}
		}
	}
}

func (c *canvas) png() []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, c.img)
	return buf.Bytes()
}

var (
	steel   = rgb(176, 188, 199)
	dark    = rgb(52, 64, 78)
	blue    = rgb(15, 148, 237)
	navy    = rgb(2, 37, 69)
	white   = rgb(240, 244, 248)
	glass   = rgb(120, 190, 235)
	orange  = rgb(255, 120, 60)
	green   = rgb(60, 180, 120)
	concret = rgb(150, 150, 145)
)

// locationImage draws a building, floor or room illustration.
func locationImage(style string) []byte {
	c := newCanvas(960, 600)
	switch style {
	case "fab", "cub":
		c.gradient(rgb(120, 180, 230), rgb(220, 236, 248))
		c.rect(0, 470, 960, 600, rgb(90, 110, 90))
		if style == "fab" {
			c.rect(120, 190, 840, 480, white)
			c.rect(120, 170, 840, 200, steel)
			for x := 150; x < 820; x += 70 {
				c.rect(x, 230, x+40, 260, glass)
			}
			c.rect(160, 120, 260, 175, steel) // roof AHUs
			c.rect(320, 120, 420, 175, steel)
			c.rect(560, 120, 660, 175, steel)
			c.rect(430, 380, 530, 480, navy) // entrance
		} else {
			c.rect(180, 240, 780, 480, concret)
			for x := 210; x < 760; x += 90 {
				c.rect(x, 280, x+50, 320, glass)
			}
			for i := 0; i < 3; i++ { // cooling towers on roof
				x := 230 + i*180
				c.rect(x, 170, x+140, 240, steel)
				c.ring(x+70, 165, 40, 8, dark)
			}
			c.rect(820, 120, 860, 480, steel) // stack
		}
	case "floor":
		c.gradient(rgb(225, 230, 236), rgb(190, 198, 206))
		for x := 60; x < 900; x += 210 {
			c.frame(x, 80, x+180, 520, 6, dark)
			c.rect(x+20, 420, x+160, 470, steel)
		}
	default: // room
		c.gradient(rgb(200, 208, 216), rgb(150, 158, 166))
		c.rect(0, 420, 960, 600, rgb(120, 130, 120))
		for x := 80; x < 900; x += 260 {
			c.rect(x, 230, x+200, 430, steel)
			c.frame(x, 230, x+200, 430, 5, dark)
			c.rect(x+20, 250, x+80, 280, green)
		}
		c.rect(0, 120, 960, 140, orange) // pipe rack
		c.rect(0, 160, 960, 175, blue)
	}
	return c.png()
}

// equipmentImage draws an equipment illustration.
func equipmentImage(kind string) []byte {
	c := newCanvas(640, 420)
	c.gradient(rgb(236, 241, 246), rgb(200, 210, 220))
	c.rect(0, 360, 640, 420, rgb(170, 176, 170))
	switch kind {
	case "chiller":
		c.rect(70, 220, 570, 300, blue) // evaporator
		c.rect(70, 140, 570, 215, rgb(40, 120, 200))
		c.rect(220, 60, 420, 140, steel) // compressor
		c.ring(320, 100, 30, 6, dark)
		c.rect(100, 300, 130, 360, dark)
		c.rect(510, 300, 540, 360, dark)
		c.rect(560, 160, 620, 180, orange)
		c.rect(560, 240, 620, 260, rgb(30, 160, 230))
	case "ct":
		c.rect(140, 120, 500, 360, steel)
		for y := 150; y < 340; y += 18 {
			c.rect(150, y, 490, y+8, rgb(150, 162, 172))
		}
		c.rect(200, 80, 440, 120, dark)
		c.ring(320, 100, 70, 10, navy)
	case "pcw", "chwp", "cwp":
		c.rect(120, 310, 520, 360, dark) // base
		c.circle(220, 240, 70, blue)     // volute
		c.circle(220, 240, 25, navy)
		c.rect(300, 200, 500, 290, rgb(30, 110, 60)) // motor
		for x := 320; x < 490; x += 18 {
			c.rect(x, 200, x+6, 290, rgb(20, 90, 50))
		}
		c.rect(180, 100, 260, 175, steel) // pipe
	case "cda":
		c.rect(100, 90, 540, 360, rgb(70, 110, 160))
		c.rect(120, 110, 300, 200, white)
		c.rect(130, 120, 220, 150, green)
		for y := 230; y < 340; y += 16 {
			c.rect(330, y, 520, y+8, rgb(50, 85, 130))
		}
	case "mau":
		c.rect(40, 130, 600, 340, steel)
		c.frame(40, 130, 600, 340, 6, dark)
		for x := 60; x < 200; x += 14 {
			c.rect(x, 150, x+6, 320, dark) // louvres
		}
		c.ring(400, 235, 80, 12, navy)
		c.circle(400, 235, 20, dark)
		c.rect(220, 150, 300, 320, rgb(120, 190, 230)) // coil
	case "chws":
		c.rect(90, 80, 550, 330, dark) // BMS plant graphic
		c.rect(105, 95, 535, 315, navy)
		c.rect(140, 135, 240, 195, steel) // chillers
		c.rect(140, 225, 240, 285, steel)
		c.rect(240, 160, 470, 170, blue)   // supply header
		c.rect(240, 250, 470, 260, orange) // return header
		c.rect(460, 160, 470, 260, green)  // bypass
		c.circle(330, 255, 16, blue)       // pump
		c.rect(300, 330, 340, 365, dark)
		c.rect(230, 360, 410, 375, dark)
	case "etch", "etchd", "cvd", "pvd", "swc", "ash", "rtp", "ecp":
		// cluster tool from above: transfer chamber with its process
		// chambers, load lock and FOUP load ports
		body := rgb(232, 236, 240)
		c.rect(250, 130, 390, 270, body) // transfer chamber
		c.frame(250, 130, 390, 270, 5, dark)
		c.circle(320, 200, 34, steel) // robot
		accent := map[string]color.RGBA{"etch": blue, "etchd": navy, "cvd": green, "pvd": orange, "swc": rgb(120, 190, 230), "ash": rgb(170, 120, 200),
			"rtp": rgb(230, 170, 60), "ecp": rgb(200, 120, 60)}[kind]
		xs := []int{110, 260, 410}
		if kind == "ash" || kind == "rtp" {
			xs = []int{185, 335}
		}
		for _, x := range xs {
			c.rect(x, 40, x+120, 120, body)
			c.frame(x, 40, x+120, 120, 5, dark)
			c.circle(x+60, 80, 24, accent)
		}
		c.rect(270, 270, 370, 320, steel) // load lock
		for x := 200; x < 440; x += 85 {
			c.rect(x, 325, x+70, 360, orange) // FOUPs
		}
	case "scanarf", "scankrf", "stepper":
		// exposure tool: long body with the projection lens column, the
		// linked track on the left
		c.rect(60, 170, 200, 350, rgb(232, 236, 240)) // track
		c.frame(60, 170, 200, 350, 5, dark)
		c.rect(200, 120, 590, 350, rgb(240, 240, 236))
		c.frame(200, 120, 590, 350, 6, dark)
		c.rect(360, 70, 430, 260, steel) // lens column
		c.circle(395, 270, 30, navy)
		c.rect(230, 150, 330, 210, navy) // screen
		c.rect(240, 160, 320, 200, glass)
		c.rect(80, 325, 150, 360, orange) // FOUP
	case "track":
		c.rect(80, 150, 560, 350, rgb(232, 236, 240))
		c.frame(80, 150, 560, 350, 6, dark)
		for x := 120; x < 520; x += 110 {
			c.circle(x+35, 230, 32, steel) // coater / developer cups
			c.circle(x+35, 230, 12, dark)
		}
		c.rect(110, 290, 530, 320, rgb(230, 170, 60)) // hot plates
	case "cmp":
		c.rect(80, 120, 560, 360, rgb(232, 236, 240))
		c.frame(80, 120, 560, 360, 6, dark)
		for _, x := range []int{180, 320, 460} {
			c.circle(x, 220, 55, rgb(200, 205, 210)) // platens
			c.circle(x, 220, 20, navy)                // heads
		}
		c.rect(120, 300, 520, 340, steel) // clean module
	case "furn":
		c.rect(220, 40, 420, 370, rgb(232, 236, 240)) // vertical furnace
		c.frame(220, 40, 420, 370, 6, dark)
		c.rect(270, 70, 370, 250, rgb(230, 120, 60)) // heater
		c.rect(290, 250, 350, 330, rgb(200, 215, 230))
		c.rect(160, 320, 220, 360, orange)
	case "impmc", "imphc":
		c.rect(60, 180, 200, 330, rgb(232, 236, 240)) // source / terminal
		c.frame(60, 180, 200, 330, 5, dark)
		c.rect(200, 240, 420, 270, steel) // beamline
		c.circle(320, 255, 50, navy)      // analyser magnet
		c.rect(420, 160, 580, 350, rgb(232, 236, 240))
		c.frame(420, 160, 580, 350, 5, dark)
		c.circle(500, 255, 36, steel) // end station
	case "wat":
		c.rect(120, 130, 520, 360, rgb(232, 236, 240))
		c.frame(120, 130, 520, 360, 6, dark)
		c.circle(260, 250, 70, steel) // chuck
		c.rect(380, 160, 500, 240, navy)
		c.rect(390, 170, 490, 230, glass)
		c.rect(380, 260, 500, 330, dark) // SMU rack
	case "scrub":
		c.rect(220, 90, 420, 360, rgb(220, 225, 230))
		c.frame(220, 90, 420, 360, 6, navy)
		c.rect(260, 130, 380, 200, rgb(230, 120, 60)) // burner
		c.rect(260, 220, 380, 330, rgb(120, 190, 230)) // water tank
		c.rect(300, 40, 340, 90, steel)                // inlet
	case "pch":
		c.rect(180, 110, 460, 360, rgb(220, 225, 230))
		c.frame(180, 110, 460, 360, 6, navy)
		c.rect(210, 140, 300, 200, navy) // controller
		c.rect(220, 150, 290, 190, glass)
		c.circle(380, 260, 45, steel) // compressor
		c.rect(210, 300, 300, 340, blue)
	case "met", "cds", "ovl", "def":
		c.rect(160, 110, 480, 360, rgb(232, 236, 240))
		c.frame(160, 110, 480, 360, 6, dark)
		c.rect(200, 150, 330, 230, navy) // screen
		c.rect(210, 160, 320, 220, glass)
		c.circle(400, 200, 40, steel) // optical head
		c.circle(400, 200, 14, dark)
		c.rect(200, 325, 280, 360, orange)
	case "wet":
		c.rect(60, 140, 580, 360, rgb(232, 236, 240))
		c.frame(60, 140, 580, 360, 6, dark)
		for x := 90; x < 560; x += 95 {
			c.rect(x, 180, x+75, 250, rgb(120, 190, 230)) // tanks
		}
		c.rect(60, 110, 580, 140, steel) // exhaust plenum
		c.rect(90, 300, 160, 340, orange)
	case "vac":
		c.rect(170, 160, 470, 360, rgb(220, 225, 230))
		c.frame(170, 160, 470, 360, 6, navy)
		c.rect(200, 190, 300, 230, blue)
		c.circle(250, 120, 30, steel) // inlet flange
		c.rect(235, 120, 265, 160, steel)
		c.rect(470, 280, 560, 300, steel) // exhaust
	}
	return c.png()
}
