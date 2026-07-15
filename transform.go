package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ─── ANSI state machine ─────────────────────────────────────────────────

type escState int

const (
	stateText escState = iota
	stateESC
	stateESCCollect // ESC followed by a prefix char like ( ) * + that needs one more byte
	stateCSI
	stateOSC
	stateESCInOSC
)

// ─── screen cell ────────────────────────────────────────────────────────

type cell struct {
	ch    rune
	valid bool
}

// ─── RainbowTransformer ─────────────────────────────────────────────────

type RainbowTransformer struct {
	mu      sync.Mutex
	dst     io.Writer

	// Animation
	phase     atomic.Int64 // fixed-point degrees * 1000
	freq      float64      // hue degrees per column
	ticker    *time.Ticker
	done      chan struct{}
	closeOnce sync.Once

	// ANSI state
	state  escState
	escBuf bytes.Buffer

	// UTF-8 sequence buffer
	utfBuf [utf8.UTFMax]byte
	utfLen int
	utfPos int

	// writeBuf is reused across Write calls to reduce allocation pressure.
	writeBuf bytes.Buffer

	// Cursor tracking (0-based)
	curRow int
	curCol int

	// Saved cursor position (DECSC / \0337)
	saveRow int
	saveCol int

	// Alternate screen tracking
	altScreen bool

	// Screen buffer (flat, row-major)
	scrRows int
	scrCols int
	cells   []cell

	// Animation mode (static content redraw)
	idleSince   time.Time
	animRunning atomic.Bool
}

func NewRainbowTransformer(dst io.Writer, speedHz, freq float64, rows, cols int, redrawHz float64) *RainbowTransformer {
	t := &RainbowTransformer{
		dst:     dst,
		freq:    freq,
		done:    make(chan struct{}),
		scrRows: rows,
		scrCols: cols,
		cells:   make([]cell, rows*cols),
	}
	if rows == 0 || cols == 0 {
		t.scrRows, t.scrCols = 24, 80
		t.cells = make([]cell, 24*80)
	}
	if speedHz > 0 {
		period := time.Duration(float64(time.Second) / speedHz)
		t.ticker = time.NewTicker(period)
		go t.animate()
	}
	// Screen buffer redraw for static-content animation.
	// Disabled by default (redrawHz == 0) to avoid flicker with TUI apps like vim.
	if redrawHz > 0 {
		go t.animLoop(redrawHz)
	}
	return t
}

func (t *RainbowTransformer) Close() {
	t.closeOnce.Do(func() {
		if t.ticker != nil {
			t.ticker.Stop()
		}
		close(t.done)
	})
}

// ─── animation goroutines ───────────────────────────────────────────────

func (t *RainbowTransformer) animate() {
	for {
		select {
		case <-t.ticker.C:
			t.phase.Add(1000) // +1 degree per tick
		case <-t.done:
			return
		}
	}
}

func (t *RainbowTransformer) getPhase() float64 {
	p := t.phase.Load()
	return float64(p%360000) / 1000.0
}

// animLoop periodically redraws the screen buffer when the child is idle.
func (t *RainbowTransformer) animLoop(redrawHz float64) {
	period := time.Duration(float64(time.Second) / redrawHz)
	tick := time.NewTicker(period)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			// Require a longer idle period (3s) before redrawing,
			// so actively-edited TUI apps like vim don't flicker.
			if time.Since(t.idleSince) > 3*time.Second &&
				t.altScreen &&
				t.scrRows > 0 && t.scrCols > 0 &&
				t.hasAnyCell() {
				t.animRunning.Store(true)
				t.redraw()
			} else {
				t.animRunning.Store(false)
			}
		case <-t.done:
			return
		}
	}
}

// redraw re-renders the entire screen buffer with current rainbow hues.
func (t *RainbowTransformer) redraw() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.hasAnyCell() {
		return
	}

	phase := t.getPhase()
	var buf bytes.Buffer

	// Save cursor position (DECSC).
	buf.WriteString("\x1b7")

	rows, cols := t.scrRows, t.scrCols
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			cell := t.cells[r*cols+c]
			if !cell.valid {
				continue
			}
			hue := phase + t.freq*(float64(r)*0.6+float64(c))
			buf.WriteString(fmt.Sprintf("\x1b[%d;%dH", r+1, c+1))
			buf.WriteString(rainbowSeq(hue))
			buf.WriteRune(cell.ch)
		}
	}

	// Restore cursor position (DECRC).
	buf.WriteString("\x1b8")

	_, _ = buf.WriteTo(t.dst)
}

func (t *RainbowTransformer) hasAnyCell() bool {
	for _, c := range t.cells {
		if c.valid {
			return true
		}
	}
	return false
}

// ─── main Write ─────────────────────────────────────────────────────────

func (t *RainbowTransformer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.idleSince = time.Now()
	t.animRunning.Store(false)

	t.writeBuf.Reset()
	out := &t.writeBuf
	phase := t.getPhase()

	for i := 0; i < len(p); i++ {
		b := p[i]

		// Finish any in-progress UTF-8 sequence first.
		if t.utfLen > 0 {
			t.utfBuf[t.utfPos] = b
			t.utfPos++
			if t.utfPos >= t.utfLen {
				r, _ := utf8.DecodeRune(t.utfBuf[:t.utfLen])
				t.emitText(out, phase, r)
				t.utfLen = 0
				t.utfPos = 0
			}
			continue
		}

		switch t.state {
		case stateText:
			t.handleTextByte(out, phase, b)
		case stateESC:
			t.escBuf.WriteByte(b)
			switch b {
			case '[':
				t.state = stateCSI
			case ']':
				t.state = stateOSC
			case '7': // DECSC – save cursor
				t.saveRow, t.saveCol = t.curRow, t.curCol
				out.Write(t.escBuf.Bytes())
				t.state = stateText
			case '8': // DECRC – restore cursor
				t.curRow, t.curCol = t.saveRow, t.saveCol
				out.Write(t.escBuf.Bytes())
				t.state = stateText
			case '(', ')', '*', '+', '-', '.', '/', '%', '"', '#', ' ':
				// Prefix that expects one following byte to complete the escape.
				t.state = stateESCCollect
			case 'M': // RI – reverse index (scroll down)
				if t.curRow == 0 {
					t.scrollDown(1)
				} else {
					t.curRow--
				}
				out.Write(t.escBuf.Bytes())
				t.state = stateText
			case 'D': // IND – index (scroll up)
				if t.curRow >= t.scrRows-1 {
					t.scrollUp(1)
				} else {
					t.curRow++
				}
				out.Write(t.escBuf.Bytes())
				t.state = stateText
			default:
				out.Write(t.escBuf.Bytes())
				t.state = stateText
			}

		case stateESCCollect:
			t.escBuf.WriteByte(b)
			out.Write(t.escBuf.Bytes())
			t.state = stateText

		case stateCSI:
			t.escBuf.WriteByte(b)
			if b >= 0x40 && b <= 0x7E {
				t.handleCSI(out, t.escBuf.Bytes(), b)
				t.state = stateText
			}

		case stateOSC:
			t.escBuf.WriteByte(b)
			switch b {
			case '\x1b':
				t.state = stateESCInOSC
			case '\a':
				out.Write(t.escBuf.Bytes())
				t.state = stateText
			}

		case stateESCInOSC:
			t.escBuf.WriteByte(b)
			if b == '\\' {
				out.Write(t.escBuf.Bytes())
			}
			t.state = stateText
		}
	}

	_, err := out.WriteTo(t.dst)
	return len(p), err
}

// ─── text handling ──────────────────────────────────────────────────────

func (t *RainbowTransformer) handleTextByte(out *bytes.Buffer, phase float64, b byte) {
	switch b {
	case '\n':
		if t.curRow >= t.scrRows-1 {
			t.scrollUp(1)
		} else {
			t.curRow++
		}
		t.curCol = 0
		out.WriteByte(b)
	case '\r':
		t.curCol = 0
		out.WriteByte(b)
	case '\t':
		tab := 8 - (t.curCol % 8)
		for j := 0; j < tab; j++ {
			if t.curCol < t.scrCols {
				t.putCell(' ', phase)
				hue := phase + t.freq*(float64(t.curRow)*0.6+float64(t.curCol))
				out.WriteString(rainbowSeq(hue))
				out.WriteByte(' ')
				t.curCol++
			}
		}
	case '\b':
		if t.curCol > 0 {
			t.curCol--
		}
		out.WriteByte(b)
	case '\x7f':
		out.WriteByte(b)
	case '\x1b':
		t.escBuf.Reset()
		t.escBuf.WriteByte(b)
		t.state = stateESC
	default:
		if b < 0x20 {
			out.WriteByte(b)
			return
		}
		// UTF-8 multi-byte start?
		if b >= 0xC0 {
			switch {
			case b&0xF8 == 0xF0:
				t.utfLen = 4
			case b&0xF0 == 0xE0:
				t.utfLen = 3
			default:
				t.utfLen = 2
			}
			t.utfBuf[0] = b
			t.utfPos = 1
			return
		}
		// ASCII printable.
		t.emitText(out, phase, rune(b))
	}
}

func (t *RainbowTransformer) emitText(out *bytes.Buffer, phase float64, r rune) {
	if t.curCol >= t.scrCols {
		if t.curRow >= t.scrRows-1 {
			t.scrollUp(1)
		} else {
			t.curRow++
		}
		t.curCol = 0
	}
	t.putCell(r, phase)
	hue := phase + t.freq*(float64(t.curRow)*0.6+float64(t.curCol))
	out.WriteString(rainbowSeq(hue))
	out.WriteRune(r)
	t.curCol += runeWidth(r)
}

// putCell stores a character in the screen buffer at (curRow, curCol).
func (t *RainbowTransformer) putCell(r rune, _ float64) {
	if t.curRow < 0 || t.curRow >= t.scrRows || t.curCol < 0 || t.curCol >= t.scrCols {
		return
	}
	idx := t.curRow*t.scrCols + t.curCol
	t.cells[idx] = cell{ch: r, valid: true}
}

// ─── scroll / erase helpers ─────────────────────────────────────────────

func (t *RainbowTransformer) scrollUp(n int) {
	if n <= 0 || t.scrRows == 0 || t.scrCols == 0 {
		return
	}
	copy(t.cells, t.cells[n*t.scrCols:])
	clear := t.cells[(t.scrRows-n)*t.scrCols:]
	for i := range clear {
		clear[i] = cell{}
	}
}

func (t *RainbowTransformer) scrollDown(n int) {
	if n <= 0 || t.scrRows == 0 || t.scrCols == 0 {
		return
	}
	copy(t.cells[n*t.scrCols:], t.cells)
	clear := t.cells[:n*t.scrCols]
	for i := range clear {
		clear[i] = cell{}
	}
}

func (t *RainbowTransformer) eraseDisplay(mode int) {
	cols := t.scrCols
	switch mode {
	case 0: // cursor → end
		start := t.curRow*cols + t.curCol
		for i := start; i < len(t.cells); i++ {
			t.cells[i] = cell{}
		}
	case 1: // start → cursor
		end := t.curRow*cols + t.curCol + 1
		for i := 0; i < end && i < len(t.cells); i++ {
			t.cells[i] = cell{}
		}
	case 2: // all
		for i := range t.cells {
			t.cells[i] = cell{}
		}
	}
}

func (t *RainbowTransformer) eraseLine(mode int) {
	cols := t.scrCols
	start := t.curRow * cols
	switch mode {
	case 0: // cursor → end
		for i := start + t.curCol; i < start+cols && i < len(t.cells); i++ {
			t.cells[i] = cell{}
		}
	case 1: // start → cursor
		for i := start; i <= start+t.curCol && i < len(t.cells); i++ {
			t.cells[i] = cell{}
		}
	case 2: // entire line
		for i := start; i < start+cols && i < len(t.cells); i++ {
			t.cells[i] = cell{}
		}
	}
}

func (t *RainbowTransformer) insertLine(n int) {
	if n <= 0 {
		return
	}
	cols := t.scrCols
	rowStart := t.curRow * cols
	copy(t.cells[rowStart+n*cols:], t.cells[rowStart:])
	for i := 0; i < n*cols && rowStart+i < len(t.cells); i++ {
		t.cells[rowStart+i] = cell{}
	}
}

func (t *RainbowTransformer) deleteLine(n int) {
	if n <= 0 {
		return
	}
	cols := t.scrCols
	rowStart := t.curRow * cols
	copy(t.cells[rowStart:], t.cells[rowStart+n*cols:])
	clear := t.cells[len(t.cells)-n*cols:]
	for i := range clear {
		clear[i] = cell{}
	}
}

// ─── CSI handler ────────────────────────────────────────────────────────

func (t *RainbowTransformer) handleCSI(out *bytes.Buffer, seq []byte, final byte) {
	params := parseCSIParams(seq)

	switch final {
	case 'A': // CUU – Cursor Up
		n := param(params, 0, 1)
		t.curRow = max(0, t.curRow-n)
		out.Write(seq)

	case 'B': // CUD – Cursor Down
		n := param(params, 0, 1)
		t.curRow = min(t.scrRows-1, t.curRow+n)
		out.Write(seq)

	case 'C': // CUF – Cursor Forward
		n := param(params, 0, 1)
		t.curCol = min(t.scrCols-1, t.curCol+n)
		out.Write(seq)

	case 'D': // CUB – Cursor Back
		n := param(params, 0, 1)
		t.curCol = max(0, t.curCol-n)
		out.Write(seq)

	case 'H', 'f': // CUP / HVP – Cursor Position
		r := param(params, 0, 1) - 1
		c := param(params, 1, 1) - 1
		t.curRow = clamp(r, 0, t.scrRows-1)
		t.curCol = clamp(c, 0, t.scrCols-1)
		out.Write(seq)

	case 'G': // CHA – Cursor Horizontal Absolute
		c := param(params, 0, 1) - 1
		t.curCol = clamp(c, 0, t.scrCols-1)
		out.Write(seq)

	case 'd': // VPA – Vertical Position Absolute
		r := param(params, 0, 1) - 1
		t.curRow = clamp(r, 0, t.scrRows-1)
		out.Write(seq)

	case 'E': // CNL – Cursor Next Line
		n := param(params, 0, 1)
		t.curRow = min(t.scrRows-1, t.curRow+n)
		t.curCol = 0
		out.Write(seq)

	case 'F': // CPL – Cursor Previous Line
		n := param(params, 0, 1)
		t.curRow = max(0, t.curRow-n)
		t.curCol = 0
		out.Write(seq)

	case 'J': // ED – Erase in Display
		mode := param(params, 0, 0)
		t.eraseDisplay(mode)
		out.Write(seq)

	case 'K': // EL – Erase in Line
		mode := param(params, 0, 0)
		t.eraseLine(mode)
		out.Write(seq)

	case 'L': // IL – Insert Line
		n := param(params, 0, 1)
		t.insertLine(n)
		out.Write(seq)

	case 'M': // DL – Delete Line
		n := param(params, 0, 1)
		t.deleteLine(n)
		out.Write(seq)

	case 'S': // SU – Scroll Up
		n := param(params, 0, 1)
		t.scrollUp(n)
		out.Write(seq)

	case 'T': // SD – Scroll Down
		n := param(params, 0, 1)
		t.scrollDown(n)
		out.Write(seq)

	case 'h': // DECSET – set mode
		seqStr := string(seq)
		if strings.Contains(seqStr, "?1049") || strings.Contains(seqStr, "?1047") {
			t.altScreen = true
			// Clear buffer when entering alt screen.
			for i := range t.cells {
				t.cells[i] = cell{}
			}
		}
		out.Write(seq)

	case 'l': // DECRST – reset mode
		seqStr := string(seq)
		if strings.Contains(seqStr, "?1049") || strings.Contains(seqStr, "?1047") {
			t.altScreen = false
			for i := range t.cells {
				t.cells[i] = cell{}
			}
		}
		out.Write(seq)

	case 'm': // SGR – strip color params, keep style params
		filtered := filterSGRParams(params)
		out.WriteString("\x1b[")
		if filtered != "" {
			out.WriteString(filtered)
		}
		out.WriteByte('m')

	default:
		out.Write(seq)
	}
}

// ─── CSI parameter helpers ──────────────────────────────────────────────

func parseCSIParams(seq []byte) []int {
	s := string(seq)
	open := strings.IndexByte(s, '[')
	if open < 0 {
		return nil
	}
	body := s[open+1 : len(s)-1]
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ";")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			v = 0
		}
		out = append(out, v)
	}
	return out
}

func param(params []int, idx, def int) int {
	if idx < len(params) {
		v := params[idx]
		if v > 0 {
			return v
		}
	}
	return def
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// filterSGRParams removes color-related SGR parameters but keeps style ones.
func filterSGRParams(params []int) string {
	if len(params) == 0 {
		return ""
	}
	var kept []int
	i := 0
	for i < len(params) {
		p := params[i]
		switch {
		case p == 0:
			kept = append(kept, p) // reset
		case p == 1, p == 3, p == 4, p == 5, p == 6, p == 7, p == 8, p == 9:
			kept = append(kept, p) // bold, italic, underline, blinks, reverse, conceal, strikethrough
		case p == 2:
			kept = append(kept, p) // dim (same param used by 38;2 sequence but context matters)
		case p >= 21 && p <= 29:
			kept = append(kept, p) // attribute resets (22=bold off, etc.)
		case p >= 51 && p <= 59:
			kept = append(kept, p) // framed, encircled, overline, etc.
		case p == 38 || p == 48:
			// Extended color: skip parameters (38;5;N or 38;2;R;G;B)
			i++
			if i < len(params) {
				mode := params[i]
				i++
				if mode == 5 && i < len(params) {
					i++ // skip palette index N
				} else if mode == 2 {
					i += 3 // skip R;G;B
				}
			}
			continue
		case p == 39 || p == 49:
			// default fg/bg – strip
		case p >= 90 && p <= 97:
			// bright foreground – strip
		case p >= 100 && p <= 107:
			// bright background – strip
		case p >= 30 && p <= 37:
			// standard foreground – strip
		case p >= 40 && p <= 47:
			// standard background – strip
		default:
			kept = append(kept, p) // pass through unknown params
		}
		i++
	}
	if len(kept) == 0 {
		return ""
	}
	parts := make([]string, len(kept))
	for i, v := range kept {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ";")
}

// runeWidth returns the terminal display width of a rune.
// CJK and fullwidth characters occupy 2 columns; most others occupy 1.
func runeWidth(r rune) int {
	if r >= 0x1100 &&
		(r <= 0x115F || r == 0x2329 || r == 0x232A ||
			(r >= 0x2E80 && r <= 0xA4CF) ||
			(r >= 0xAC00 && r <= 0xD7AF) ||
			(r >= 0xF900 && r <= 0xFAFF) ||
			(r >= 0xFE10 && r <= 0xFE19) ||
			(r >= 0xFE30 && r <= 0xFE6F) ||
			(r >= 0xFF01 && r <= 0xFF60) ||
			(r >= 0xFFE0 && r <= 0xFFE6) ||
			(r >= 0x1B000 && r <= 0x1B0FF) ||
			(r >= 0x1D000 && r <= 0x1F1FF) ||
			(r >= 0x20000 && r <= 0x2FFFF) ||
			(r >= 0x30000 && r <= 0x3FFFF)) {
		return 2
	}
	return 1
}
