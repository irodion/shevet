// vtbench2: same acceptance tests for danielgatis/go-headless-term
// (separate module: its go-ansicode version conflicts with midterm's).
package main

import (
	"fmt"
	"strings"

	headlessterm "github.com/danielgatis/go-headless-term"
)

type hemu struct{ t *headlessterm.Terminal }

func newH(w, h int) *hemu {
	return &hemu{headlessterm.New(
		headlessterm.WithSize(h, w),
		headlessterm.WithScrollback(headlessterm.NewMemoryScrollback(10000)),
	)}
}
func (h *hemu) write(s string) { h.t.WriteString(s) }
func (h *hemu) cell(x, y int) (string, bool, string) {
	c := h.t.Cell(y, x)
	if c == nil {
		return "<nil>", false, ""
	}
	if c.Fg == nil {
		return string(c.Char), false, ""
	}
	r, g, b, _ := c.Fg.RGBA()
	return string(c.Char), true, fmt.Sprintf("rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
}
func (h *hemu) rowText(y, w int) string {
	var sb strings.Builder
	for x := 0; x < w; x++ {
		c := h.t.Cell(y, x)
		if c == nil || c.Char == 0 || c.Char == ' ' {
			sb.WriteString("·")
		} else {
			sb.WriteRune(c.Char)
		}
	}
	return sb.String()
}

func safely(fn func() string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("PANIC: %v", r)
		}
	}()
	return fn()
}

func main() {
	report := func(name string, fn func() string) {
		fmt.Printf("%-16s | %s\n", name, safely(fn))
	}

	report("plain", func() string {
		e := newH(20, 6)
		e.write("hi")
		c, _, _ := e.cell(0, 0)
		return c
	})
	report("truecolor fg", func() string {
		e := newH(20, 6)
		e.write("\x1b[38;2;255;100;0mX")
		c, has, fg := e.cell(0, 0)
		return fmt.Sprintf("%s has=%v %s", c, has, fg)
	})
	report("256color fg", func() string {
		e := newH(20, 6)
		e.write("\x1b[38;5;196mY")
		c, has, fg := e.cell(0, 0)
		return fmt.Sprintf("%s has=%v %s", c, has, fg)
	})
	report("split escape", func() string {
		e := newH(20, 6)
		e.write("\x1b[38;2;")
		e.write("0;255;0mZ")
		c, has, fg := e.cell(0, 0)
		return fmt.Sprintf("%s has=%v %s", c, has, fg)
	})
	report("CJK width", func() string {
		e := newH(20, 6)
		e.write("你A")
		c0, _, _ := e.cell(0, 0)
		c1, _, _ := e.cell(1, 0)
		c2, _, _ := e.cell(2, 0)
		return fmt.Sprintf("[0]=%q [1]=%q [2]=%q", c0, c1, c2)
	})
	report("emoji cluster", func() string {
		e := newH(20, 6)
		e.write("👩‍🚀B")
		c0, _, _ := e.cell(0, 0)
		c1, _, _ := e.cell(1, 0)
		c2, _, _ := e.cell(2, 0)
		return fmt.Sprintf("[0]=%q [1]=%q [2]=%q", c0, c1, c2)
	})
	report("alt screen", func() string {
		e := newH(20, 6)
		e.write("main")
		e.write("\x1b[?1049h\x1b[HALT-CONTENT")
		mid, _, _ := e.cell(0, 0)
		e.write("\x1b[?1049l")
		back, _, _ := e.cell(0, 0)
		return fmt.Sprintf("alt=%q restored=%q", mid, back)
	})
	report("scroll region", func() string {
		e := newH(20, 6)
		e.write("ROW1\r\nROW2\r\nROW3\r\nROW4\r\nROW5\r\nROW6")
		e.write("\x1b[2;5r")
		e.write("\x1b[5;1H\n\n")
		return fmt.Sprintf("top=%q row2=%q bottom=%q", e.rowText(0, 6), e.rowText(1, 6), e.rowText(5, 6))
	})
	report("insert line IL", func() string {
		e := newH(20, 6)
		e.write("AAA\r\nBBB\r\nCCC")
		e.write("\x1b[2;1H\x1b[1L")
		return fmt.Sprintf("row2=%q row3=%q", e.rowText(1, 4), e.rowText(2, 4))
	})
	report("scrollback", func() string {
		e := newH(20, 6)
		for i := 1; i <= 20; i++ {
			e.write(fmt.Sprintf("line%02d\r\n", i))
		}
		return fmt.Sprintf("len=%d", e.t.ScrollbackLen())
	})
	report("resize", func() string {
		e := newH(20, 6)
		e.write("RESIZE-TEST")
		e.t.Resize(3, 10)
		e.t.Resize(8, 30)
		c, _, _ := e.cell(0, 0)
		return fmt.Sprintf("ok cell0=%q", c)
	})
	report("dirty cells", func() string {
		e := newH(20, 6)
		e.write("dirty\r\nrows")
		return fmt.Sprintf("DirtyCells=%d HasDirty=%v", len(e.t.DirtyCells()), e.t.HasDirty())
	})
}
