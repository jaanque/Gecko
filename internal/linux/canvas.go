package linux

import (
	"bytes"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"

	"remote-control/internal/clientui"
	"remote-control/internal/i18n"
)

type uiRect struct {
	x0, y0, x1, y1 int
}

func (r uiRect) contains(x, y int) bool {
	return x >= r.x0 && x < r.x1 && y >= r.y0 && y < r.y1
}

const (
	clientWinW = 420
	clientWinH = 215
)

type uiButtonID int

const (
	btnNone uiButtonID = iota
	btnAllow
	btnDeny
	btnDisconnect
)

type clientCanvas struct {
	w, h    int
	bgra    []byte // top-down 32-bit BGRA for direct X11 PutImage
	logo    *image.NRGBA
	strings i18n.ClientStrings
}

func newClientCanvas(logoPNG []byte, lang i18n.Lang) *clientCanvas {
	c := &clientCanvas{
		w:       clientWinW,
		h:       clientWinH,
		bgra:    make([]byte, clientWinW*clientWinH*4),
		strings: i18n.Client(lang),
	}
	if len(logoPNG) > 0 {
		if img, _, err := image.Decode(bytes.NewReader(logoPNG)); err == nil {
			b := img.Bounds()
			nrgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
			for y := 0; y < b.Dy(); y++ {
				for x := 0; x < b.Dx(); x++ {
					nrgba.Set(x, y, img.At(b.Min.X+x, b.Min.Y+y))
				}
			}
			c.logo = nrgba
		}
	}
	return c
}

func (c *clientCanvas) buttonRects() (allowRC, denyRC, discRC uiRect) {
	const pad = 24
	const gap = 12
	const btnY = 150
	const btnH = 38
	halfW := (c.w - pad*2 - gap) / 2
	allowRC = uiRect{pad, btnY, pad + halfW, btnY + btnH}
	denyRC = uiRect{pad + halfW + gap, btnY, pad + halfW + gap + halfW, btnY + btnH}
	const discW = 132
	discRC = uiRect{c.w - pad - discW, 146, c.w - pad, 146 + 36}
	return allowRC, denyRC, discRC
}

func (c *clientCanvas) hitTest(state clientui.State, x, y int) uiButtonID {
	allowRC, denyRC, discRC := c.buttonRects()
	switch state {
	case clientui.StatePrompt:
		if allowRC.contains(x, y) {
			return btnAllow
		}
		if denyRC.contains(x, y) {
			return btnDeny
		}
	case clientui.StateActive:
		if discRC.contains(x, y) {
			return btnDisconnect
		}
	}
	return btnNone
}

func (c *clientCanvas) render(hostname string, state clientui.State, detail string, hover uiButtonID) {
	// Crisp white background.
	c.fillRect(uiRect{0, 0, c.w, c.h}, color.RGBA{255, 255, 255, 255})

	const pad = 24
	const headerY = 26
	textLeft := pad

	if c.logo != nil {
		const logoSz = 44
		c.drawLogoFit(uiRect{pad, headerY, pad + logoSz, headerY + logoSz})
		textLeft = pad + logoSz + 14
	}

	// Hostname (bold 2x crisp text).
	maxHostW := c.w - pad - textLeft
	c.drawTextEllipsis(textLeft, headerY+6, maxHostW, hostname, 2, true, color.RGBA{24, 24, 27, 255})

	// Subtle divider line.
	c.fillRect(uiRect{pad, 88, c.w - pad, 89}, color.RGBA{228, 228, 231, 255})

	allowRC, denyRC, discRC := c.buttonRects()

	switch state {
	case clientui.StatePrompt:
		c.drawTextEllipsis(pad, 110, c.w-pad*2, c.strings.PromptMessage, 1, false, color.RGBA{24, 24, 27, 255})

		allowBg := color.RGBA{24, 24, 27, 255}
		if hover == btnAllow {
			allowBg = color.RGBA{44, 44, 48, 255}
		}
		c.drawButton(allowRC, 6, allowBg, allowBg, color.RGBA{255, 255, 255, 255}, c.strings.Allow)

		denyBg := color.RGBA{255, 255, 255, 255}
		if hover == btnDeny {
			denyBg = color.RGBA{244, 244, 245, 255}
		}
		c.drawButton(denyRC, 6, denyBg, color.RGBA{212, 212, 216, 255}, color.RGBA{24, 24, 27, 255}, c.strings.Deny)

	case clientui.StateActive:
		c.drawCircle(pad+5, 164, 5, color.RGBA{22, 163, 74, 255})
		c.drawTextEllipsis(pad+20, 156, discRC.x0-pad-28, c.strings.ActiveSession, 1, false, color.RGBA{24, 24, 27, 255})

		discBg := color.RGBA{255, 255, 255, 255}
		if hover == btnDisconnect {
			discBg = color.RGBA{244, 244, 245, 255}
		}
		c.drawButton(discRC, 6, discBg, color.RGBA{212, 212, 216, 255}, color.RGBA{24, 24, 27, 255}, c.strings.Disconnect)

	default:
		dotCol := color.RGBA{217, 119, 6, 255}
		statusTxt := detail
		switch state {
		case clientui.StateOnline:
			dotCol = color.RGBA{22, 163, 74, 255}
			if statusTxt == "" {
				statusTxt = c.strings.Online
			}
		case clientui.StateError:
			dotCol = color.RGBA{220, 38, 38, 255}
			if statusTxt == "" {
				statusTxt = c.strings.ErrorFallback
			}
		default:
			if statusTxt == "" {
				statusTxt = c.strings.Connecting
			}
		}
		c.drawCircle(pad+5, 146, 5, dotCol)
		c.drawTextEllipsis(pad+20, 138, c.w-pad*2-20, statusTxt, 1, false, color.RGBA{82, 82, 91, 255})
	}
}

func (c *clientCanvas) setPixelBlend(x, y int, col color.RGBA, alpha float64) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h || alpha <= 0 {
		return
	}
	if alpha > 1 {
		alpha = 1
	}
	off := (y*c.w + x) * 4
	if alpha >= 0.999 {
		c.bgra[off+0] = col.B
		c.bgra[off+1] = col.G
		c.bgra[off+2] = col.R
		c.bgra[off+3] = 255
		return
	}
	inv := 1.0 - alpha
	c.bgra[off+0] = uint8(float64(col.B)*alpha + float64(c.bgra[off+0])*inv + 0.5)
	c.bgra[off+1] = uint8(float64(col.G)*alpha + float64(c.bgra[off+1])*inv + 0.5)
	c.bgra[off+2] = uint8(float64(col.R)*alpha + float64(c.bgra[off+2])*inv + 0.5)
	c.bgra[off+3] = 255
}

func (c *clientCanvas) fillRect(r uiRect, col color.RGBA) {
	x0 := max(0, r.x0)
	y0 := max(0, r.y0)
	x1 := min(c.w, r.x1)
	y1 := min(c.h, r.y1)
	for y := y0; y < y1; y++ {
		off := (y*c.w + x0) * 4
		for x := x0; x < x1; x++ {
			c.bgra[off+0] = col.B
			c.bgra[off+1] = col.G
			c.bgra[off+2] = col.R
			c.bgra[off+3] = 255
			off += 4
		}
	}
}

func (c *clientCanvas) drawCircle(cx, cy, radius int, col color.RGBA) {
	rf := float64(radius)
	for y := cy - radius - 1; y <= cy+radius+1; y++ {
		for x := cx - radius - 1; x <= cx+radius+1; x++ {
			dx := float64(x - cx)
			dy := float64(y - cy)
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist <= rf-0.5 {
				c.setPixelBlend(x, y, col, 1.0)
			} else if dist < rf+0.5 {
				c.setPixelBlend(x, y, col, rf+0.5-dist)
			}
		}
	}
}

func (c *clientCanvas) drawButton(r uiRect, radius int, bg, border, fg color.RGBA, label string) {
	rf := float64(radius)
	for y := r.y0; y < r.y1; y++ {
		for x := r.x0; x < r.x1; x++ {
			// Signed distance to rounded rectangle.
			dx := 0.0
			if x < r.x0+radius {
				dx = float64(r.x0+radius-x) - 0.5
			} else if x >= r.x1-radius {
				dx = float64(x-(r.x1-radius)) + 0.5
			}
			dy := 0.0
			if y < r.y0+radius {
				dy = float64(r.y0+radius-y) - 0.5
			} else if y >= r.y1-radius {
				dy = float64(y-(r.y1-radius)) + 0.5
			}
			var dist float64
			if dx > 0 && dy > 0 {
				dist = math.Sqrt(dx*dx+dy*dy) - rf
			} else {
				// Distance to nearest straight edge.
				dLeft := float64(r.x0 - x)
				dRight := float64(x - (r.x1 - 1))
				dTop := float64(r.y0 - y)
				dBot := float64(y - (r.y1 - 1))
				dist = max(max(dLeft, dRight), max(dTop, dBot))
			}

			if dist > 0.5 {
				continue
			}
			cov := 1.0
			if dist > -0.5 {
				cov = 0.5 - dist
			}
			if dist > -1.2 {
				c.setPixelBlend(x, y, border, cov)
			} else {
				c.setPixelBlend(x, y, bg, cov)
			}
		}
	}

	tw := measureText(label, 1)
	tx := r.x0 + (r.x1-r.x0-tw)/2
	ty := r.y0 + (r.y1-r.y0-14)/2
	c.drawText(tx, ty, label, 1, true, fg)
}

func (c *clientCanvas) drawLogoFit(dst uiRect) {
	if c.logo == nil {
		return
	}
	sb := c.logo.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return
	}
	dw, dh := dst.x1-dst.x0, dst.y1-dst.y0
	if dw <= 0 || dh <= 0 {
		return
	}

	scale := math.Min(float64(dw)/float64(sw), float64(dh)/float64(sh))
	rw := max(1, int(float64(sw)*scale+0.5))
	rh := max(1, int(float64(sh)*scale+0.5))
	ox := dst.x0 + (dw-rw)/2
	oy := dst.y0 + (dh-rh)/2

	for y := 0; y < rh; y++ {
		sy := (float64(y)+0.5)*float64(sh)/float64(rh) - 0.5
		if sy < 0 {
			sy = 0
		}
		if sy > float64(sh-1) {
			sy = float64(sh - 1)
		}
		y0 := int(sy)
		y1 := min(y0+1, sh-1)
		fy := sy - float64(y0)

		for x := 0; x < rw; x++ {
			sx := (float64(x)+0.5)*float64(sw)/float64(rw) - 0.5
			if sx < 0 {
				sx = 0
			}
			if sx > float64(sw-1) {
				sx = float64(sw - 1)
			}
			x0 := int(sx)
			x1 := min(x0+1, sw-1)
			fx := sx - float64(x0)

			c00 := c.logo.NRGBAAt(x0, y0)
			c10 := c.logo.NRGBAAt(x1, y0)
			c01 := c.logo.NRGBAAt(x0, y1)
			c11 := c.logo.NRGBAAt(x1, y1)

			lerp := func(a, b, d, e uint8) float64 {
				top := float64(a)*(1-fx) + float64(b)*fx
				bot := float64(d)*(1-fx) + float64(e)*fx
				return top*(1-fy) + bot*fy
			}
			af := lerp(c00.A, c10.A, c01.A, c11.A) / 255.0
			if af <= 0.01 {
				continue
			}
			r := uint8(lerp(c00.R, c10.R, c01.R, c11.R) + 0.5)
			g := uint8(lerp(c00.G, c10.G, c01.G, c11.G) + 0.5)
			b := uint8(lerp(c00.B, c10.B, c01.B, c11.B) + 0.5)
			c.setPixelBlend(ox+x, oy+y, color.RGBA{r, g, b, 255}, af)
		}
	}
}

// --- Crisp 7x14 Bitmap Font (ASCII + Catalan diacritics) ---

const (
	glyphW = 7
	glyphH = 14
)

func measureText(s string, scale int) int {
	n := 0
	for range s {
		n++
	}
	if n == 0 {
		return 0
	}
	return n * (glyphW + 1) * scale
}

func (c *clientCanvas) drawTextEllipsis(x, y, maxW int, s string, scale int, bold bool, col color.RGBA) {
	if measureText(s, scale) <= maxW {
		c.drawText(x, y, s, scale, bold, col)
		return
	}
	runes := []rune(s)
	adv := (glyphW + 1) * scale
	maxChars := maxW / adv
	if maxChars <= 3 {
		return
	}
	if maxChars < len(runes) {
		runes = append(runes[:maxChars-3], '.', '.', '.')
	}
	c.drawText(x, y, string(runes), scale, bold, col)
}

func (c *clientCanvas) drawText(x, y int, s string, scale int, bold bool, col color.RGBA) {
	cx := x
	adv := (glyphW + 1) * scale
	for _, r := range s {
		rows := lookupGlyph(r)
		for row := 0; row < glyphH; row++ {
			bits := rows[row]
			if bits == 0 {
				continue
			}
			for colIdx := 0; colIdx < glyphW; colIdx++ {
				if (bits & (1 << (glyphW - 1 - colIdx))) != 0 {
					px := cx + colIdx*scale
					py := y + row*scale
					for sy := 0; sy < scale; sy++ {
						for sx := 0; sx < scale; sx++ {
							c.setPixelBlend(px+sx, py+sy, col, 1.0)
						}
					}
					if bold {
						for sy := 0; sy < scale; sy++ {
							c.setPixelBlend(px+scale, py+sy, col, 0.75)
						}
					}
				}
			}
		}
		cx += adv
	}
}

func lookupGlyph(r rune) [glyphH]uint8 {
	if g, ok := baseGlyphs[r]; ok {
		return g
	}
	// Compose Catalan / Latin-1 accented glyphs cleanly from base ASCII glyph + accent mark.
	var base rune
	var accent string
	switch r {
	case 'à':
		base, accent = 'a', "grave"
	case 'á':
		base, accent = 'a', "acute"
	case 'è':
		base, accent = 'e', "grave"
	case 'é':
		base, accent = 'e', "acute"
	case 'í':
		base, accent = 'i', "acute_i"
	case 'ï':
		base, accent = 'i', "umlaut_i"
	case 'ò':
		base, accent = 'o', "grave"
	case 'ó':
		base, accent = 'o', "acute"
	case 'ú':
		base, accent = 'u', "acute"
	case 'ü':
		base, accent = 'u', "umlaut"
	case 'ç':
		base, accent = 'c', "cedilla"
	case 'À':
		base, accent = 'A', "grave_up"
	case 'Á':
		base, accent = 'A', "acute_up"
	case 'È':
		base, accent = 'E', "grave_up"
	case 'É':
		base, accent = 'E', "acute_up"
	case 'Í':
		base, accent = 'I', "acute_up"
	case 'Ï':
		base, accent = 'I', "umlaut_up"
	case 'Ò':
		base, accent = 'O', "grave_up"
	case 'Ó':
		base, accent = 'O', "acute_up"
	case 'Ú':
		base, accent = 'U', "acute_up"
	case 'Ü':
		base, accent = 'U', "umlaut_up"
	case 'Ç':
		base, accent = 'C', "cedilla"
	case 'ñ':
		base, accent = 'n', "tilde"
	case 'Ñ':
		base, accent = 'N', "tilde_up"
	default:
		return baseGlyphs['?']
	}

	g := baseGlyphs[base]
	switch accent {
	case "grave":
		g[2] |= 0b0010000
		g[3] |= 0b0001000
	case "acute":
		g[2] |= 0b0000100
		g[3] |= 0b0001000
	case "acute_i":
		g[2] = 0b0000100
		g[3] = 0b0001000
	case "umlaut_i":
		g[2] = 0
		g[3] = 0b0100010
	case "umlaut":
		g[3] |= 0b0100010
	case "cedilla":
		g[12] |= 0b0001000
		g[13] |= 0b0011000
	case "tilde":
		g[2] |= 0b0110100
		g[3] |= 0b0001000
	case "grave_up":
		g[0] |= 0b0010000
		g[1] |= 0b0001000
	case "acute_up":
		g[0] |= 0b0000100
		g[1] |= 0b0001000
	case "umlaut_up":
		g[0] |= 0b0100010
	case "tilde_up":
		g[0] |= 0b0110100
	}
	return g
}

// 7x14 crisp sans-serif glyphs (bit 6 = leftmost pixel, bit 0 = rightmost pixel).
// Rows 2..11 are cap height; rows 5..11 are x-height; rows 12..13 are descenders.
var baseGlyphs = map[rune][glyphH]uint8{
	' ':  {},
	'!':  {0, 0, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0, 0x08, 0x08, 0, 0},
	'"':  {0, 0, 0x14, 0x14, 0x14, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	'#':  {0, 0, 0, 0x14, 0x14, 0x3E, 0x14, 0x14, 0x3E, 0x14, 0x14, 0, 0, 0},
	'$':  {0, 0, 0x08, 0x1E, 0x28, 0x28, 0x1C, 0x0A, 0x0A, 0x3C, 0x08, 0, 0, 0},
	'%':  {0, 0, 0x22, 0x52, 0x24, 0x08, 0x08, 0x10, 0x24, 0x4A, 0x44, 0, 0, 0},
	'&':  {0, 0, 0x18, 0x24, 0x24, 0x18, 0x28, 0x46, 0x44, 0x44, 0x3A, 0, 0, 0},
	'\'': {0, 0, 0x08, 0x08, 0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	'(':  {0, 0, 0x04, 0x08, 0x10, 0x10, 0x10, 0x10, 0x10, 0x08, 0x04, 0, 0, 0},
	')':  {0, 0, 0x10, 0x08, 0x04, 0x04, 0x04, 0x04, 0x04, 0x08, 0x10, 0, 0, 0},
	'*':  {0, 0, 0, 0, 0x08, 0x2A, 0x1C, 0x2A, 0x08, 0, 0, 0, 0, 0},
	'+':  {0, 0, 0, 0, 0x08, 0x08, 0x3E, 0x08, 0x08, 0, 0, 0, 0, 0},
	',':  {0, 0, 0, 0, 0, 0, 0, 0, 0, 0x08, 0x08, 0x10, 0, 0},
	'-':  {0, 0, 0, 0, 0, 0, 0, 0x3E, 0, 0, 0, 0, 0, 0},
	'.':  {0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x18, 0x18, 0, 0},
	'·':  {0, 0, 0, 0, 0, 0, 0x18, 0x18, 0, 0, 0, 0, 0, 0},
	'/':  {0, 0, 0x02, 0x02, 0x04, 0x04, 0x08, 0x08, 0x10, 0x10, 0x20, 0x20, 0, 0},
	'0':  {0, 0, 0x1C, 0x22, 0x41, 0x43, 0x45, 0x49, 0x51, 0x61, 0x22, 0x1C, 0, 0},
	'1':  {0, 0, 0x08, 0x18, 0x28, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x3E, 0, 0},
	'2':  {0, 0, 0x1C, 0x22, 0x41, 0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x7F, 0, 0},
	'3':  {0, 0, 0x1C, 0x22, 0x41, 0x01, 0x0E, 0x01, 0x01, 0x41, 0x22, 0x1C, 0, 0},
	'4':  {0, 0, 0x04, 0x0C, 0x14, 0x24, 0x44, 0x7F, 0x04, 0x04, 0x04, 0x04, 0, 0},
	'5':  {0, 0, 0x7F, 0x40, 0x40, 0x5C, 0x62, 0x01, 0x01, 0x41, 0x22, 0x1C, 0, 0},
	'6':  {0, 0, 0x1C, 0x22, 0x40, 0x40, 0x5C, 0x62, 0x41, 0x41, 0x22, 0x1C, 0, 0},
	'7':  {0, 0, 0x7F, 0x01, 0x02, 0x04, 0x04, 0x08, 0x08, 0x10, 0x10, 0x10, 0, 0},
	'8':  {0, 0, 0x1C, 0x22, 0x41, 0x22, 0x1C, 0x22, 0x41, 0x41, 0x22, 0x1C, 0, 0},
	'9':  {0, 0, 0x1C, 0x22, 0x41, 0x41, 0x23, 0x1D, 0x01, 0x01, 0x22, 0x1C, 0, 0},
	':':  {0, 0, 0, 0, 0, 0x18, 0x18, 0, 0, 0, 0x18, 0x18, 0, 0},
	';':  {0, 0, 0, 0, 0, 0x18, 0x18, 0, 0, 0, 0x08, 0x08, 0x10, 0},
	'<':  {0, 0, 0, 0, 0x04, 0x08, 0x10, 0x20, 0x10, 0x08, 0x04, 0, 0, 0},
	'=':  {0, 0, 0, 0, 0, 0x3E, 0, 0, 0x3E, 0, 0, 0, 0, 0},
	'>':  {0, 0, 0, 0, 0x20, 0x10, 0x08, 0x04, 0x08, 0x10, 0x20, 0, 0, 0},
	'?':  {0, 0, 0x1C, 0x22, 0x41, 0x01, 0x02, 0x08, 0x08, 0, 0x08, 0x08, 0, 0},
	'@':  {0, 0, 0x1C, 0x22, 0x41, 0x4D, 0x55, 0x55, 0x4E, 0x40, 0x22, 0x1C, 0, 0},
	'A':  {0, 0, 0x08, 0x14, 0x14, 0x22, 0x22, 0x41, 0x7F, 0x41, 0x41, 0x41, 0, 0},
	'B':  {0, 0, 0x7C, 0x42, 0x41, 0x42, 0x7C, 0x42, 0x41, 0x41, 0x42, 0x7C, 0, 0},
	'C':  {0, 0, 0x1C, 0x22, 0x41, 0x40, 0x40, 0x40, 0x40, 0x41, 0x22, 0x1C, 0, 0},
	'D':  {0, 0, 0x78, 0x44, 0x42, 0x41, 0x41, 0x41, 0x41, 0x42, 0x44, 0x78, 0, 0},
	'E':  {0, 0, 0x7F, 0x40, 0x40, 0x40, 0x7C, 0x40, 0x40, 0x40, 0x40, 0x7F, 0, 0},
	'F':  {0, 0, 0x7F, 0x40, 0x40, 0x40, 0x7C, 0x40, 0x40, 0x40, 0x40, 0x40, 0, 0},
	'G':  {0, 0, 0x1C, 0x22, 0x41, 0x40, 0x40, 0x4F, 0x41, 0x41, 0x22, 0x1E, 0, 0},
	'H':  {0, 0, 0x41, 0x41, 0x41, 0x41, 0x7F, 0x41, 0x41, 0x41, 0x41, 0x41, 0, 0},
	'I':  {0, 0, 0x3E, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x3E, 0, 0},
	'J':  {0, 0, 0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x44, 0x44, 0x38, 0, 0},
	'K':  {0, 0, 0x41, 0x42, 0x44, 0x48, 0x70, 0x48, 0x44, 0x42, 0x41, 0x41, 0, 0},
	'L':  {0, 0, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x7F, 0, 0},
	'M':  {0, 0, 0x41, 0x63, 0x55, 0x49, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0, 0},
	'N':  {0, 0, 0x41, 0x61, 0x51, 0x49, 0x45, 0x43, 0x41, 0x41, 0x41, 0x41, 0, 0},
	'O':  {0, 0, 0x1C, 0x22, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x22, 0x1C, 0, 0},
	'P':  {0, 0, 0x7C, 0x42, 0x41, 0x41, 0x42, 0x7C, 0x40, 0x40, 0x40, 0x40, 0, 0},
	'Q':  {0, 0, 0x1C, 0x22, 0x41, 0x41, 0x41, 0x41, 0x49, 0x45, 0x22, 0x1D, 0, 0},
	'R':  {0, 0, 0x7C, 0x42, 0x41, 0x41, 0x42, 0x7C, 0x48, 0x44, 0x42, 0x41, 0, 0},
	'S':  {0, 0, 0x1E, 0x21, 0x40, 0x40, 0x3C, 0x02, 0x01, 0x01, 0x42, 0x3C, 0, 0},
	'T':  {0, 0, 0x7F, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0, 0},
	'U':  {0, 0, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x22, 0x1C, 0, 0},
	'V':  {0, 0, 0x41, 0x41, 0x41, 0x41, 0x22, 0x22, 0x22, 0x14, 0x14, 0x08, 0, 0},
	'W':  {0, 0, 0x41, 0x41, 0x41, 0x41, 0x41, 0x49, 0x49, 0x55, 0x63, 0x41, 0, 0},
	'X':  {0, 0, 0x41, 0x41, 0x22, 0x14, 0x08, 0x08, 0x14, 0x22, 0x41, 0x41, 0, 0},
	'Y':  {0, 0, 0x41, 0x41, 0x22, 0x14, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0, 0},
	'Z':  {0, 0, 0x7F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x40, 0x7F, 0, 0},
	'[':  {0, 0, 0x1C, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1C, 0, 0},
	'\\': {0, 0, 0x20, 0x20, 0x10, 0x10, 0x08, 0x08, 0x04, 0x04, 0x02, 0x02, 0, 0},
	']':  {0, 0, 0x1C, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x1C, 0, 0},
	'^':  {0, 0, 0x08, 0x14, 0x22, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	'_':  {0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x7F, 0},
	'`':  {0, 0, 0x10, 0x08, 0x04, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	'a':  {0, 0, 0, 0, 0, 0x1C, 0x22, 0x02, 0x1E, 0x22, 0x22, 0x1F, 0, 0},
	'b':  {0, 0, 0x40, 0x40, 0x40, 0x5C, 0x62, 0x41, 0x41, 0x41, 0x62, 0x5C, 0, 0},
	'c':  {0, 0, 0, 0, 0, 0x1E, 0x21, 0x40, 0x40, 0x40, 0x21, 0x1E, 0, 0},
	'd':  {0, 0, 0x01, 0x01, 0x01, 0x1D, 0x23, 0x41, 0x41, 0x41, 0x23, 0x1D, 0, 0},
	'e':  {0, 0, 0, 0, 0, 0x1C, 0x22, 0x41, 0x7F, 0x40, 0x21, 0x1E, 0, 0},
	'f':  {0, 0, 0x0C, 0x12, 0x10, 0x3C, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0, 0},
	'g':  {0, 0, 0, 0, 0, 0x1D, 0x23, 0x41, 0x41, 0x41, 0x23, 0x1D, 0x01, 0x3E},
	'h':  {0, 0, 0x40, 0x40, 0x40, 0x5C, 0x62, 0x41, 0x41, 0x41, 0x41, 0x41, 0, 0},
	'i':  {0, 0, 0, 0x08, 0, 0x18, 0x08, 0x08, 0x08, 0x08, 0x08, 0x1C, 0, 0},
	'j':  {0, 0, 0, 0x04, 0, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x04, 0x44, 0x44, 0x38},
	'k':  {0, 0, 0x40, 0x40, 0x40, 0x42, 0x44, 0x48, 0x70, 0x48, 0x44, 0x42, 0, 0},
	'l':  {0, 0, 0x18, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x1C, 0, 0},
	'm':  {0, 0, 0, 0, 0, 0x6A, 0x55, 0x49, 0x49, 0x49, 0x49, 0x49, 0, 0},
	'n':  {0, 0, 0, 0, 0, 0x5C, 0x62, 0x41, 0x41, 0x41, 0x41, 0x41, 0, 0},
	'o':  {0, 0, 0, 0, 0, 0x1C, 0x22, 0x41, 0x41, 0x41, 0x22, 0x1C, 0, 0},
	'p':  {0, 0, 0, 0, 0, 0x5C, 0x62, 0x41, 0x41, 0x41, 0x62, 0x5C, 0x40, 0x40},
	'q':  {0, 0, 0, 0, 0, 0x1D, 0x23, 0x41, 0x41, 0x41, 0x23, 0x1D, 0x01, 0x01},
	'r':  {0, 0, 0, 0, 0, 0x5C, 0x62, 0x40, 0x40, 0x40, 0x40, 0x40, 0, 0},
	's':  {0, 0, 0, 0, 0, 0x1E, 0x21, 0x20, 0x1C, 0x02, 0x42, 0x3C, 0, 0},
	't':  {0, 0, 0, 0x10, 0x10, 0x3C, 0x10, 0x10, 0x10, 0x10, 0x12, 0x0C, 0, 0},
	'u':  {0, 0, 0, 0, 0, 0x41, 0x41, 0x41, 0x41, 0x41, 0x43, 0x3D, 0, 0},
	'v':  {0, 0, 0, 0, 0, 0x41, 0x41, 0x22, 0x22, 0x14, 0x14, 0x08, 0, 0},
	'w':  {0, 0, 0, 0, 0, 0x41, 0x41, 0x49, 0x49, 0x49, 0x55, 0x22, 0, 0},
	'x':  {0, 0, 0, 0, 0, 0x41, 0x22, 0x14, 0x08, 0x14, 0x22, 0x41, 0, 0},
	'y':  {0, 0, 0, 0, 0, 0x41, 0x41, 0x22, 0x22, 0x14, 0x14, 0x08, 0x10, 0x60},
	'z':  {0, 0, 0, 0, 0, 0x7F, 0x02, 0x04, 0x08, 0x10, 0x20, 0x7F, 0, 0},
	'{':  {0, 0, 0x06, 0x08, 0x08, 0x08, 0x30, 0x08, 0x08, 0x08, 0x08, 0x06, 0, 0},
	'|':  {0, 0, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0, 0},
	'}':  {0, 0, 0x30, 0x08, 0x08, 0x08, 0x06, 0x08, 0x08, 0x08, 0x08, 0x30, 0, 0},
	'~':  {0, 0, 0, 0, 0, 0x32, 0x4C, 0, 0, 0, 0, 0, 0, 0},
}
