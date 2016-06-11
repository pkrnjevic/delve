package textwnd

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"strings"
	"sync"

	"github.com/derekparker/delve/terminal/textwnd/internal/assets"
	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/exp/shiny/driver"
	"golang.org/x/exp/shiny/screen"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"golang.org/x/mobile/event/key"
	"golang.org/x/mobile/event/lifecycle"
	"golang.org/x/mobile/event/mouse"
	"golang.org/x/mobile/event/paint"
	"golang.org/x/mobile/event/size"
)

//go:generate go-bindata -o internal/assets/assets.go -pkg assets ProggyClean.ttf

type Theme struct {
	FontSize            int
	Fg, Bg, ScrollColor color.Color
	HlFg                color.Color
	SearchFg, SearchBg  color.Color
	Height, Width       int
}

type Wnd struct {
	screen    screen.Screen
	wnd       screen.Window
	wndb      screen.Buffer
	img       *image.RGBA
	bounds    image.Rectangle
	uilock    sync.Mutex
	outevents chan interface{}
	theme     Theme
	face      font.Face

	lines      []string
	pos        int
	xoff       int
	xoffinc    int
	maxlinelen int

	searchstr                    []rune
	searchon                     bool
	searchline                   int
	searchcolstart, searchcolend int
}

type MouseEvent struct {
	Direction mouse.Direction
	Button    mouse.Button
	Line      int
	Col       int
}

var assetsOnce sync.Once
var ttfontDefault *truetype.Font

func getFontFace(size int) font.Face {
	assetsOnce.Do(func() {
		fontData, _ := assets.Asset("ProggyClean.ttf")
		ttfontDefault, _ = freetype.ParseFont(fontData)
	})
	return truetype.NewFace(ttfontDefault, &truetype.Options{Size: float64(size), Hinting: font.HintingFull, DPI: 96})
}

func NewWindow(theme Theme, lines []string, centerOn int) *Wnd {
	face := getFontFace(theme.FontSize)
	wnd := &Wnd{theme: theme, lines: lines, face: face}
	go driver.Main(func(s screen.Screen) { wnd.main(s, centerOn) })
	return wnd
}

func (wnd *Wnd) Events() <-chan interface{} {
	wnd.uilock.Lock()
	defer wnd.uilock.Unlock()
	wnd.outevents = make(chan interface{})
	return wnd.outevents
}

func (wnd *Wnd) numberOfLines() int {
	return int(wnd.bounds.Dy() / wnd.face.Metrics().Height.Floor())
}

func (wnd *Wnd) clampPos(p int) {
	wnd.pos = p
	if wnd.pos < 0 {
		wnd.pos = 0
	}
	if wnd.pos >= len(wnd.lines) {
		wnd.pos = len(wnd.lines) - 1
	}
}

func (wnd *Wnd) clampXoff(xoff int) {
	wnd.xoff = xoff
	if wnd.xoff < 0 {
		wnd.xoff = 0
	}
	if wnd.xoff >= wnd.maxlinelen-(wnd.bounds.Dx()/2) {
		wnd.xoff = wnd.maxlinelen - wnd.bounds.Dx()/2
		if wnd.xoff < 0 {
			wnd.xoff = 0
		}
	}
}

func expandTabs(in string) string {
	hastab := false
	for _, c := range in {
		if c == '\t' {
			hastab = true
			break
		}
	}
	if !hastab {
		return in
	}

	var buf bytes.Buffer
	count := 0
	for _, c := range in {
		if c == '\t' {
			d := (((count/8)+1)*8 - count)
			for i := 0; i < d; i++ {
				buf.WriteRune(' ')
			}
		} else {
			buf.WriteRune(c)
			count++
		}
	}
	return buf.String()
}

func (wnd *Wnd) Redraw(lines []string, centerOn int, alwaysCenter bool) {
	wnd.uilock.Lock()
	defer wnd.uilock.Unlock()

	wnd.lines = lines
	for i := range wnd.lines {
		wnd.lines[i] = expandTabs(wnd.lines[i])
	}
	nln := wnd.numberOfLines()

	if centerOn >= 0 {
		if alwaysCenter || centerOn < wnd.pos || centerOn >= wnd.pos+nln {
			wnd.xoff = 0
			wnd.clampPos(centerOn - nln/2)
		}
	}

	wnd.updateLocked()
}

func (wnd *Wnd) main(s screen.Screen, centerOn int) {
	var err error

	wnd.screen = s
	if wnd.theme.Height == 0 {
		wnd.theme.Height = 480
	}
	if wnd.theme.Width == 0 {
		wnd.theme.Width = 640
	}
	wnd.wnd, err = s.NewWindow(&screen.NewWindowOptions{wnd.theme.Width, wnd.theme.Height})
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not create window: %v", err)
		return
	}
	wnd.setupBuffer(image.Point{wnd.theme.Width, wnd.theme.Height})
	wnd.Redraw(wnd.lines, centerOn, true)

	for {
		ei := wnd.wnd.NextEvent()
		wnd.uilock.Lock()
		r := wnd.handleEventLocked(ei)
		wnd.uilock.Unlock()
		if !r {
			break
		}
	}
}

func (wnd *Wnd) close() {
	if wnd.wndb != nil {
		wnd.wndb.Release()
	}
	wnd.wnd.Release()
	if wnd.outevents != nil {
		close(wnd.outevents)
		wnd.outevents = nil
	}
}

func (wnd *Wnd) handleEventLocked(ei interface{}) bool {
	switch e := ei.(type) {
	case paint.Event:
		wnd.updateLocked()
	case lifecycle.Event:
		if e.To == lifecycle.StageDead {
			wnd.close()
			return false
		}
	case size.Event:
		sz := e.Size()
		bb := wnd.wndb.Bounds()
		if sz.X <= bb.Dx() && sz.Y <= bb.Dy() {
			wnd.bounds = wnd.wndb.Bounds()
			wnd.bounds.Max.Y = wnd.bounds.Min.Y + sz.Y
			wnd.bounds.Max.X = wnd.bounds.Min.X + sz.X
			wnd.updateLocked()
		} else {
			if wnd.wndb != nil {
				wnd.wndb.Release()
			}
			wnd.setupBuffer(sz)
			wnd.updateLocked()
		}

	case mouse.Event:
		switch e.Direction {
		case mouse.DirPress, mouse.DirRelease:
			switch e.Button {
			case mouse.ButtonWheelUp:
				if e.Direction == mouse.DirPress {
					wnd.clampPos(wnd.pos - 1)
					wnd.updateLocked()
				}
			case mouse.ButtonWheelDown:
				if e.Direction == mouse.DirPress {
					wnd.clampPos(wnd.pos + 1)
					wnd.updateLocked()
				}
			default:
				if wnd.outevents != nil {
					l, c := wnd.coord2pos(e.X, e.Y)
					wnd.outevents <- MouseEvent{e.Direction, e.Button, l, c}
				}
			}
		}
	case key.Event:
		if wnd.searchon {
			if e.Direction == key.DirPress {
				switch e.Code {
				case key.CodeEscape:
					wnd.searchon = false
					wnd.updateLocked()
					return true
				case key.CodeDeleteBackspace:
					if e.Modifiers == 0 {
						if len(wnd.searchstr) != 0 {
							wnd.searchstr = wnd.searchstr[:len(wnd.searchstr)-1]
							wnd.updateLocked()
						}
						return true
					}
				case key.CodeG:
					if e.Modifiers == key.ModControl {
						wnd.fwdlook(true)
						return true
					}
				}

				if e.Rune > 0 && e.Modifiers == 0 {
					wnd.searchstr = append(wnd.searchstr, e.Rune)
					wnd.fwdlook(false)
				}
			}
			return true
		}

		if e.Direction == key.DirPress {
			switch e.Code {
			case key.CodeUpArrow:
				if e.Modifiers == 0 {
					wnd.clampPos(wnd.pos - 1)
					wnd.updateLocked()
					return true
				}
			case key.CodeDownArrow:
				if e.Modifiers == 0 {
					wnd.clampPos(wnd.pos + 1)
					wnd.updateLocked()
					return true
				}
			case key.CodeRightArrow:
				if e.Modifiers == 0 {
					wnd.clampXoff(wnd.xoff + wnd.xoffinc)
					wnd.updateLocked()
					return true
				}
			case key.CodeLeftArrow:
				if e.Modifiers == 0 {
					wnd.clampXoff(wnd.xoff - wnd.xoffinc)
					wnd.updateLocked()
					return true
				}
			case key.CodeSpacebar, key.CodePageDown:
				if e.Modifiers == 0 {
					wnd.clampPos(wnd.pos + wnd.numberOfLines()/2)
					wnd.updateLocked()
					return true
				}
			case key.CodePageUp:
				if e.Modifiers == 0 {
					wnd.clampPos(wnd.pos - wnd.numberOfLines()/2)
					wnd.updateLocked()
					return true
				}
			case key.CodeHome:
				if e.Modifiers == 0 {
					wnd.pos = 0
					wnd.updateLocked()
					return true
				}
			case key.CodeEnd:
				if e.Modifiers == 0 {
					wnd.clampPos(len(wnd.lines) - wnd.numberOfLines())
					wnd.updateLocked()
					return true
				}
			case key.CodeF:
				if e.Modifiers&key.ModControl != 0 {
					wnd.searchon = true
					wnd.searchstr = []rune{}
					wnd.searchline = 0
					wnd.searchcolstart = 0
					wnd.searchcolend = 0
					wnd.updateLocked()
					return true
				}
			case key.CodeG:
				if e.Modifiers&key.ModControl != 0 {
					wnd.searchon = true
					wnd.fwdlook(true)
					return true
				}
			case key.CodeQ:
				if e.Modifiers == 0 {
					wnd.close()
					return true
				}
			}
		}

		if wnd.outevents != nil {
			wnd.outevents <- e
		}
	}

	return true
}

func (wnd *Wnd) coord2pos(x, y float32) (int, int) {
	//TODO: implement
	return 0, 0
}

func (wnd *Wnd) fwdlook(fromend bool) {
	defer func() {
		if wnd.searchline >= 0 && (wnd.searchline < wnd.pos || wnd.searchline >= wnd.pos+wnd.numberOfLines()) {
			wnd.xoff = 0
			wnd.clampPos(wnd.searchline)
		}
		wnd.updateLocked()
	}()

	if wnd.searchline < 0 {
		wnd.searchline = 0
		wnd.searchcolstart = 0
		wnd.searchcolend = 0
	}
	startline, startcol := wnd.searchline, wnd.searchcolstart
	if fromend {
		startcol = wnd.searchcolend
	}

	needle := string(wnd.searchstr)

	wnd.searchline = -1

	for line := startline; line < len(wnd.lines); line++ {
		off := 0
		if line == startline {
			off = startcol
		}
		idx := strings.Index(wnd.lines[line][off:], needle)
		if idx >= 0 {
			wnd.searchline = line
			wnd.searchcolstart = off + idx
			wnd.searchcolend = wnd.searchcolstart + len(needle)
			return
		}
	}
}

func (wnd *Wnd) updateLocked() {
	b := wnd.bounds
	draw.Draw(wnd.img, b, image.NewUniform(wnd.theme.Bg), b.Min, draw.Src)
	drawer := font.Drawer{Src: image.NewUniform(wnd.theme.Fg), Face: wnd.face}
	scrollwidth := drawer.MeasureString("M").Floor()

	wnd.maxlinelen = 0

	// Draw text
	{
		textbounds := b
		textbounds.Max.X -= scrollwidth
		drawer.Dst = wnd.img.SubImage(textbounds).(*image.RGBA)
		h := wnd.face.Metrics().Height
		startx := fixed.I(wnd.bounds.Min.X-wnd.xoff) + drawer.MeasureString(" ")
		wnd.xoffinc = drawer.MeasureString(" ").Floor() * 4
		drawer.Dot = fixed.Point26_6{startx, fixed.I(wnd.bounds.Min.Y) + h}
		limit := fixed.I(wnd.bounds.Max.Y) + h
		for i := wnd.pos; i < len(wnd.lines); i++ {
			if drawer.Dot.Y > limit {
				break
			}
			if wnd.searchon && i == wnd.searchline {
				drawer.DrawString(wnd.lines[i][:wnd.searchcolstart])
				drawer.Src = image.NewUniform(wnd.theme.HlFg)
				drawer.DrawString(wnd.lines[i][wnd.searchcolstart:wnd.searchcolend])
				drawer.Src = image.NewUniform(wnd.theme.Fg)
				if wnd.searchcolend < len(wnd.lines[i]) {
					drawer.DrawString(wnd.lines[i][wnd.searchcolend:])
				}
			} else {
				drawer.DrawString(wnd.lines[i])
			}
			if adv := (drawer.Dot.X - startx).Floor(); adv > wnd.maxlinelen {
				wnd.maxlinelen = adv
			}
			drawer.Dot.X = startx
			drawer.Dot.Y += h
		}
	}

	horizscroll := wnd.maxlinelen > (b.Dx() - scrollwidth)

	// Draw vertical scrollbar
	nln := wnd.numberOfLines()
	if nln < len(wnd.lines) {
		scrollbounds := b
		scrollbounds.Min.X = b.Max.X - scrollwidth
		if wnd.searchon {
			scrollbounds.Max.Y -= wnd.face.Metrics().Height.Floor()
		}
		if horizscroll {
			scrollbounds.Max.Y -= scrollwidth
			draw.Draw(wnd.img, scrollbounds, image.NewUniform(wnd.theme.Bg), scrollbounds.Min, draw.Src)
		}

		wh := scrollbounds.Dy()
		scrollheight := int(float64(nln) / float64(len(wnd.lines)) * float64(wh))
		scrolloff := int(float64(wnd.pos) / float64(len(wnd.lines)) * float64(wh))

		scrollrect := scrollbounds
		scrollrect.Min.Y += scrolloff
		scrollrect.Max.Y = scrollrect.Min.Y + scrollheight
		scrollrect = scrollrect.Intersect(scrollbounds)

		draw.Draw(wnd.img, scrollrect, image.NewUniform(wnd.theme.ScrollColor), scrollrect.Min, draw.Src)
	}

	// Draw horizontal scrollbar
	if horizscroll {
		scrollbounds := b
		scrollbounds.Min.Y = scrollbounds.Max.Y - scrollwidth
		if wnd.searchon {
			h := wnd.face.Metrics().Height.Floor()
			scrollbounds.Min.Y -= h
			scrollbounds.Max.Y -= h
		}
		scrollbounds.Max.X -= scrollwidth
		draw.Draw(wnd.img, scrollbounds, image.NewUniform(wnd.theme.Bg), scrollbounds.Min, draw.Src)

		ww := scrollbounds.Dx()
		scrolllen := int(float64(ww) / float64(wnd.maxlinelen) * float64(ww))
		scrolloff := int(float64(wnd.xoff) / float64(wnd.maxlinelen) * float64(ww))

		scrollrect := scrollbounds
		scrollrect.Min.X += scrolloff
		scrollrect.Max.X = scrollrect.Min.X + scrolllen
		scrollrect = scrollrect.Intersect(scrollbounds)

		draw.Draw(wnd.img, scrollrect, image.NewUniform(wnd.theme.ScrollColor), scrollrect.Min, draw.Src)
	}

	// Draw search prompt
	if wnd.searchon {
		promptbounds := b
		promptbounds.Min.Y = promptbounds.Max.Y - wnd.face.Metrics().Height.Floor()
		draw.Draw(wnd.img, promptbounds, image.NewUniform(wnd.theme.SearchBg), promptbounds.Min, draw.Src)
		drawer.Src = image.NewUniform(wnd.theme.SearchFg)
		drawer.Dot = fixed.Point26_6{drawer.MeasureString(" "), fixed.I(promptbounds.Max.Y) - wnd.face.Metrics().Descent}
		drawer.DrawString("Search: ")
		drawer.DrawString(string(wnd.searchstr))
	}

	draw.Draw(wnd.wndb.RGBA(), b, wnd.img, b.Min, draw.Src)
	wnd.wnd.Upload(b.Min, wnd.wndb, b)
	wnd.wnd.Publish()
}

func (wnd *Wnd) setupBuffer(sz image.Point) {
	var err error
	oldb := wnd.wndb
	wnd.wndb, err = wnd.screen.NewBuffer(sz)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not setup buffer: %v", err)
		wnd.wndb = oldb
	}
	wnd.img = image.NewRGBA(wnd.wndb.Bounds())
	wnd.bounds = wnd.wndb.Bounds()
}
