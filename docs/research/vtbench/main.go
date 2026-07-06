// vtbench: behavioral smoke test of three pure-Go VT emulators against
// Shevet's acceptance criteria (ARCHITECTURE.md §8).
package main

import (
	"fmt"
	"os"
	"image/color"
	"strings"

	vaxis "git.sr.ht/~rockorager/vaxis"
	vterm "git.sr.ht/~rockorager/vaxis/widgets/term"
	vt "github.com/charmbracelet/x/vt"
	vt10x "github.com/hinshun/vt10x"
	midterm "github.com/vito/midterm"
)

// emu is the minimal surface each adapter must expose for the tests.
type emu interface {
	write(s string)
	cell(x, y int) (content string, hasFG bool, fg string) // rendered cell + fg color info
	rowText(y, w int) string
	cursor() (x, y int)
	resize(w, h int)
	scrollbackLen() int
}

// ---------- charmbracelet/x/vt ----------

type charmEmu struct{ e *vt.Emulator }

func newCharm(w, h int) *charmEmu { return &charmEmu{vt.NewEmulator(w, h)} }
func (c *charmEmu) write(s string) { c.e.WriteString(s) }
func (c *charmEmu) cell(x, y int) (string, bool, string) {
	cl := c.e.CellAt(x, y)
	if cl == nil {
		return "<nil>", false, ""
	}
	if cl.Style.Fg == nil {
		return cl.Content, false, ""
	}
	r, g, b, _ := cl.Style.Fg.RGBA()
	return cl.Content, true, fmt.Sprintf("rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
}
func (c *charmEmu) rowText(y, w int) string {
	var sb strings.Builder
	for x := 0; x < w; x++ {
		cl := c.e.CellAt(x, y)
		if cl == nil || cl.Content == "" {
			sb.WriteString("·")
		} else {
			sb.WriteString(cl.Content)
		}
	}
	return sb.String()
}
func (c *charmEmu) cursor() (int, int) { p := c.e.CursorPosition(); return p.X, p.Y }
func (c *charmEmu) resize(w, h int)    { c.e.Resize(w, h) }
func (c *charmEmu) scrollbackLen() int { return c.e.ScrollbackLen() }

// ---------- hinshun/vt10x ----------

type vt10xEmu struct{ t vt10x.Terminal }

func newVt10x(w, h int) *vt10xEmu {
	return &vt10xEmu{vt10x.New(vt10x.WithSize(w, h))}
}
func (v *vt10xEmu) write(s string) { v.t.Write([]byte(s)) }
func (v *vt10xEmu) cell(x, y int) (string, bool, string) {
	v.t.Lock()
	defer v.t.Unlock()
	g := v.t.Cell(x, y)
	// vt10x packs truecolor as 1<<24|r<<16|g<<8|b? DefaultFG is 1<<24+iota — inspect raw.
	if g.FG == vt10x.DefaultFG {
		return string(g.Char), false, ""
	}
	if g.FG < 256 {
		return string(g.Char), true, fmt.Sprintf("idx(%d)", g.FG)
	}
	return string(g.Char), true, fmt.Sprintf("rgb(%d,%d,%d)", (g.FG>>16)&0xff, (g.FG>>8)&0xff, g.FG&0xff)
}
func (v *vt10xEmu) rowText(y, w int) string {
	v.t.Lock()
	defer v.t.Unlock()
	var sb strings.Builder
	for x := 0; x < w; x++ {
		g := v.t.Cell(x, y)
		if g.Char == 0 || g.Char == ' ' {
			sb.WriteString("·")
		} else {
			sb.WriteRune(g.Char)
		}
	}
	return sb.String()
}
func (v *vt10xEmu) cursor() (int, int) {
	v.t.Lock()
	defer v.t.Unlock()
	c := v.t.Cursor()
	return c.X, c.Y
}
func (v *vt10xEmu) resize(w, h int)    { v.t.Resize(w, h) }
func (v *vt10xEmu) scrollbackLen() int { return -1 } // no scrollback API

// ---------- vito/midterm ----------

type midtermEmu struct{ t *midterm.Terminal }

func newMidterm(w, h int) *midtermEmu { return &midtermEmu{midterm.NewTerminal(h, w)} }
func (m *midtermEmu) write(s string)  { m.t.Write([]byte(s)) }
func (m *midtermEmu) cell(x, y int) (string, bool, string) {
	if y >= len(m.t.Content) || x >= len(m.t.Content[y]) {
		return "<oob>", false, ""
	}
	ch := m.t.Content[y][x]
	var f midterm.Format
	col := 0
	for reg := range m.t.Format.Regions(y) {
		next := col + reg.Size
		if x >= col && x < next {
			f = reg.F
			break
		}
		col = next
	}
	if f.Fg == nil {
		return string(ch), false, ""
	}
	return string(ch), true, fmt.Sprintf("%v", f.Fg)
}
func (m *midtermEmu) rowText(y, w int) string {
	var sb strings.Builder
	if y >= len(m.t.Content) {
		return "<oob>"
	}
	for x := 0; x < w && x < len(m.t.Content[y]); x++ {
		ch := m.t.Content[y][x]
		if ch == 0 || ch == ' ' {
			sb.WriteString("·")
		} else {
			sb.WriteRune(ch)
		}
	}
	return sb.String()
}
func (m *midtermEmu) cursor() (int, int) { return m.t.Cursor.X, m.t.Cursor.Y }
func (m *midtermEmu) resize(w, h int)    { m.t.Resize(h, w) }
func (m *midtermEmu) scrollbackLen() int { return -1 } // append-only model differs; checked separately

// ---------- rockorager/vaxis widgets/term ----------

type vaxisEmu struct{ m *vterm.Model }

func newVaxis(w, h int) *vaxisEmu {
	m := vterm.New()
	m.Resize(w, h)
	return &vaxisEmu{m}
}
func (v *vaxisEmu) write(s string) { v.m.WriteString(s) }
func (v *vaxisEmu) find(x, y int) *vaxis.Cell {
	snap := v.m.Snapshot()
	for i := range snap.Cells {
		if snap.Cells[i].Col == x && snap.Cells[i].Row == y {
			return &snap.Cells[i].Cell
		}
	}
	return nil
}
func (v *vaxisEmu) cell(x, y int) (string, bool, string) {
	c := v.find(x, y)
	if c == nil {
		return "<none>", false, ""
	}
	if c.Style.Foreground == 0 {
		return c.Character.Grapheme, false, ""
	}
	return c.Character.Grapheme, true, fmt.Sprintf("params%v", c.Style.Foreground.Params())
}
func (v *vaxisEmu) rowText(y, w int) string {
	var sb strings.Builder
	for x := 0; x < w; x++ {
		c := v.find(x, y)
		if c == nil || c.Character.Grapheme == "" || c.Character.Grapheme == " " {
			sb.WriteString("·")
		} else {
			sb.WriteString(c.Character.Grapheme)
		}
	}
	return sb.String()
}
func (v *vaxisEmu) cursor() (int, int) { s := v.m.Snapshot(); return s.CursorCol, s.CursorRow }
func (v *vaxisEmu) resize(w, h int)    { v.m.Resize(w, h) }
func (v *vaxisEmu) scrollbackLen() int { return -1 } // not exported on the widget

// ---------- tests ----------

type result struct{ name string; cols [4]string }

var libNames = [4]string{"charm x/vt", "vt10x", "midterm", "vaxis/term"}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "perf" {
		perfMain()
	}
	var results []result
	run := func(name string, fn func(e emu, kind string) string) {
		r := result{name: name}
		makers := [4]func() emu{
			func() emu { return newCharm(20, 6) },
			func() emu { return newVt10x(20, 6) },
			func() emu { return newMidterm(20, 6) },
			func() emu { return newVaxis(20, 6) },
		}
		for i, mk := range makers {
			i, mk := i, mk
			r.cols[i] = safely(func() string { return fn(mk(), libNames[i]) })
		}
		results = append(results, r)
	}

	// 1. plain text
	run("plain", func(e emu, _ string) string {
		e.write("hi")
		c, _, _ := e.cell(0, 0)
		return c
	})

	// 2. SGR truecolor
	run("truecolor fg", func(e emu, _ string) string {
		e.write("\x1b[38;2;255;100;0mX")
		c, has, fg := e.cell(0, 0)
		return fmt.Sprintf("%s has=%v %s", c, has, fg)
	})

	// 3. SGR 256-color
	run("256color fg", func(e emu, _ string) string {
		e.write("\x1b[38;5;196mY")
		c, has, fg := e.cell(0, 0)
		return fmt.Sprintf("%s has=%v %s", c, has, fg)
	})

	// 4. escape split across two Writes (streaming!)
	run("split escape", func(e emu, _ string) string {
		e.write("\x1b[38;2;")
		e.write("0;255;0mZ")
		c, has, fg := e.cell(0, 0)
		return fmt.Sprintf("%s has=%v %s", c, has, fg)
	})

	// 5. CJK wide char
	run("CJK width", func(e emu, _ string) string {
		e.write("你A")
		c0, _, _ := e.cell(0, 0)
		c1, _, _ := e.cell(1, 0)
		c2, _, _ := e.cell(2, 0)
		return fmt.Sprintf("[0]=%q [1]=%q [2]=%q", c0, c1, c2)
	})

	// 6. emoji (ZWJ cluster)
	run("emoji cluster", func(e emu, _ string) string {
		e.write("👩‍🚀B")
		c0, _, _ := e.cell(0, 0)
		c1, _, _ := e.cell(1, 0)
		c2, _, _ := e.cell(2, 0)
		return fmt.Sprintf("[0]=%q [1]=%q [2]=%q", c0, c1, c2)
	})

	// 7. alt screen round-trip
	run("alt screen", func(e emu, _ string) string {
		e.write("main")
		e.write("\x1b[?1049h\x1b[HALT-CONTENT")
		mid, _, _ := e.cell(0, 0)
		e.write("\x1b[?1049l")
		back, _, _ := e.cell(0, 0)
		return fmt.Sprintf("alt=%q restored=%q", mid, back)
	})

	// 8. DECSTBM scroll region: rows 2-5 (1-based); row 1 must survive scrolling
	run("scroll region", func(e emu, _ string) string {
		e.write("ROW1\r\nROW2\r\nROW3\r\nROW4\r\nROW5\r\nROW6")
		e.write("\x1b[2;5r")    // region rows 2..5
		e.write("\x1b[5;1H\n\n") // LF at region bottom scrolls region twice
		top := e.rowText(0, 6)
		r2 := e.rowText(1, 6)
		bot := e.rowText(5, 6)
		e.write("\x1b[r")
		return fmt.Sprintf("top=%q row2=%q bottom=%q", top, r2, bot)
	})

	// 9. insert line (IL)
	run("insert line IL", func(e emu, _ string) string {
		e.write("AAA\r\nBBB\r\nCCC")
		e.write("\x1b[2;1H\x1b[1L") // insert 1 line at row 2
		r1 := e.rowText(1, 4)
		r2 := e.rowText(2, 4)
		return fmt.Sprintf("row2=%q row3=%q", r1, r2)
	})

	// 10. scrollback after overflow (20 lines into 6-row screen)
	run("scrollback", func(e emu, _ string) string {
		for i := 1; i <= 20; i++ {
			e.write(fmt.Sprintf("line%02d\r\n", i))
		}
		return fmt.Sprintf("len=%d", e.scrollbackLen())
	})

	// 11. resize down then up (no panic; content sanity)
	run("resize", func(e emu, _ string) string {
		e.write("RESIZE-TEST")
		e.resize(10, 3)
		e.resize(30, 8)
		c, _, _ := e.cell(0, 0)
		return fmt.Sprintf("ok cell0=%q", c)
	})

	// print table
	fmt.Printf("%-16s", "TEST")
	for _, n := range libNames {
		fmt.Printf(" | %-33s", n)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("-", 16+4*36))
	for _, r := range results {
		fmt.Printf("%-16s", r.name)
		for _, c := range r.cols {
			fmt.Printf(" | %-33s", trunc(c))
		}
		fmt.Println()
	}

	// damage APIs (lib-specific, not comparable via the common interface)
	fmt.Println("\n--- damage/dirty APIs ---")
	ce := newCharm(20, 6)
	ce.write("dirty")
	fmt.Printf("charm Touched() lines: %d (nil-safe: %v)\n", len(ce.e.Touched()), ce.e.Touched() != nil)
	mt := newMidterm(20, 6)
	mt.write("dirty\r\nrows")
	fmt.Printf("midterm Changes per row: %v\n", mt.t.Changes)
	fmt.Println("vt10x: coarse ChangeFlag only (ChangedScreen/ChangedTitle), no per-cell/per-row damage")
	fmt.Println("vaxis/term: internal dirty flag only; Snapshot() is the exported read path")

	_ = color.Black
}

func safely(fn func() string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("PANIC: %v", r)
		}
	}()
	return fn()
}

func trunc(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 38 {
		return s[:35] + "..."
	}
	return s
}
