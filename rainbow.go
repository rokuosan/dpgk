package main

import "fmt"

// hsvToRGB converts HSV (hue 0-360, sat/val 0.0-1.0) to RGB (0-255).
func hsvToRGB(hue float64, sat, val float64) (r, g, b int) {
	h := hue / 360.0
	s := sat
	v := val

	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)

	var r0, g0, b0 float64
	switch i % 6 {
	case 0:
		r0, g0, b0 = v, t, p
	case 1:
		r0, g0, b0 = q, v, p
	case 2:
		r0, g0, b0 = p, v, t
	case 3:
		r0, g0, b0 = p, q, v
	case 4:
		r0, g0, b0 = t, p, v
	case 5:
		r0, g0, b0 = v, p, q
	}

	r = int(r0 * 255)
	g = int(g0 * 255)
	b = int(b0 * 255)
	return
}

// rainbowSeq returns an ANSI truecolor SGR sequence for a hue.
func rainbowSeq(hue float64) string {
	r, g, b := hsvToRGB(hue, 1.0, 1.0)
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
}

// resetSeq returns the ANSI reset sequence.
const resetSeq = "\x1b[0m"
