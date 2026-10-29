// bastion-assets generates the WinPE background image (winpe.jpg):
// a dark blue gradient with the Bastion shield.
//
//	go run ./cmd/bastion-assets -out rescue/winpe/winpe.jpg
package main

import (
	"flag"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math"
	"os"
)

type pt struct{ x, y float64 }

func inside(p pt, poly []pt) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.y > p.y) != (b.y > p.y) && p.x < (b.x-a.x)*(p.y-a.y)/(b.y-a.y)+a.x {
			in = !in
		}
	}
	return in
}

func distSeg(p, a, b pt) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	t := ((p.x-a.x)*dx + (p.y-a.y)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy))
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	f := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 255}
}

func main() {
	out := flag.String("out", "winpe.jpg", "Zieldatei")
	w := flag.Int("w", 1920, "Breite")
	h := flag.Int("h", 1080, "Höhe")
	flag.Parse()

	img := image.NewRGBA(image.Rect(0, 0, *w, *h))
	top, bottom := color.RGBA{14, 22, 48, 255}, color.RGBA{6, 10, 24, 255}
	glow := color.RGBA{60, 84, 190, 255}
	cx, cy := float64(*w)*0.5, float64(*h)*0.42
	// shield outline (unit coordinates, scaled)
	s := float64(*h) * 0.30
	shape := []pt{}
	for i := 0; i <= 40; i++ { // rounded bottom: two curves from the sides to the tip
		t := float64(i) / 40
		shape = append(shape, pt{cx - s*0.5 + s*0.5*t*t, cy + s*0.05 + s*0.62*math.Sin(t*math.Pi/2)})
	}
	for i := 40; i >= 0; i-- {
		t := float64(i) / 40
		shape = append(shape, pt{cx + s*0.5 - s*0.5*t*t, cy + s*0.05 + s*0.62*math.Sin(t*math.Pi/2)})
	}
	shape = append(shape, pt{cx + s*0.5, cy - s*0.35}, pt{cx, cy - s*0.55}, pt{cx - s*0.5, cy - s*0.35})
	check := []pt{{cx - s*0.2, cy + s*0.02}, {cx - s*0.04, cy + s*0.18}, {cx + s*0.24, cy - s*0.16}}
	shieldA, shieldB := color.RGBA{110, 134, 255, 255}, color.RGBA{61, 87, 214, 255}

	for y := 0; y < *h; y++ {
		base := mix(top, bottom, float64(y)/float64(*h))
		for x := 0; x < *w; x++ {
			p := pt{float64(x), float64(y)}
			c := base
			// soft glow behind the shield
			d := math.Hypot(p.x-cx, (p.y-cy)*1.2) / (s * 1.6)
			if d < 1 {
				c = mix(c, glow, 0.35*(1-d)*(1-d))
			}
			if inside(p, shape) {
				c = mix(shieldA, shieldB, (p.y-(cy-s*0.55))/(s*1.2))
				if distSeg(p, check[0], check[1]) < s*0.045 || distSeg(p, check[1], check[2]) < s*0.045 {
					c = color.RGBA{255, 255, 255, 255}
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		log.Fatal(err)
	}
}
