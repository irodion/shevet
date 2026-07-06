package main

import (
	"fmt"
	"os"
	"time"
)

// perfMain: feed ~repeats colored+cursor-heavy lines into each emulator at 80x24
// and report throughput. Simulates an agent streaming styled output.
func perfMain() {
	const repeats = 50000
	line := "\x1b[38;5;39m▶\x1b[0m \x1b[1mtool\x1b[0m \x1b[38;2;120;200;90mok\x1b[0m some fairly long output line with 中文 text and paths /usr/local/lib/thing.go:123\r\n"
	payload := make([]byte, 0, len(line)*100)
	for i := 0; i < 100; i++ {
		payload = append(payload, line...)
	}
	total := int64(len(payload)) * int64(repeats/100)

	{
		e := newCharm(80, 24)
		start := time.Now()
		for i := 0; i < repeats/100; i++ {
			e.e.Write(payload)
		}
		d := time.Since(start)
		fmt.Printf("charm x/vt : %8.1f MB in %6s = %7.1f MB/s\n", float64(total)/1e6, d.Round(time.Millisecond), float64(total)/1e6/d.Seconds())
	}
	{
		e := newVt10x(80, 24)
		start := time.Now()
		for i := 0; i < repeats/100; i++ {
			e.t.Write(payload)
		}
		d := time.Since(start)
		fmt.Printf("vt10x      : %8.1f MB in %6s = %7.1f MB/s\n", float64(total)/1e6, d.Round(time.Millisecond), float64(total)/1e6/d.Seconds())
	}
	{
		e := newMidterm(80, 24)
		start := time.Now()
		for i := 0; i < repeats/100; i++ {
			e.t.Write(payload)
		}
		d := time.Since(start)
		fmt.Printf("midterm    : %8.1f MB in %6s = %7.1f MB/s\n", float64(total)/1e6, d.Round(time.Millisecond), float64(total)/1e6/d.Seconds())
	}
	os.Exit(0)
}
