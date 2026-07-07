package scriptedagent

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, script string) *Script {
	t.Helper()
	s, err := Parse(strings.NewReader(script))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func TestRun_PrintAndPrompt(t *testing.T) {
	s := parse(t, `
# a comment, then output
print building...
prompt Proceed? [y/n] `)

	var out strings.Builder
	code, err := s.Run(strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (implicit)", code)
	}
	want := "building...\nProceed? [y/n] "
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRun_AwaitLineBlocksUntilInput(t *testing.T) {
	s := parse(t, `
print before
await-line
print after
exit 7`)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := s.Run(inR, outW)
		outW.Close()
		done <- result{code, err}
	}()

	out := &lineReader{r: outR}
	if got := out.line(t); got != "before" {
		t.Fatalf("first line = %q, want %q", got, "before")
	}

	// The agent must now be parked on await-line: no further output may
	// exist before we provide input. Feed the line and expect the rest.
	if _, err := inW.Write([]byte("go\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if got := out.line(t); got != "after" {
		t.Fatalf("second line = %q, want %q", got, "after")
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		if r.code != 7 {
			t.Errorf("exit code = %d, want 7", r.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after script completed")
	}
}

func TestRun_AwaitLineFailsWhenStdinCloses(t *testing.T) {
	s := parse(t, "await-line")

	inR, inW := io.Pipe()
	inW.Close() // driver goes away

	code, err := s.Run(inR, io.Discard)
	if err == nil {
		t.Fatal("Run succeeded with closed stdin at await-line, want error")
	}
	if code == 0 {
		t.Error("exit code = 0 for a failed await-line, want non-zero")
	}
}

func TestRun_ExitStopsScript(t *testing.T) {
	s := parse(t, `
exit 3
print unreachable`)

	var out strings.Builder
	code, err := s.Run(strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if out.Len() != 0 {
		t.Errorf("output after exit = %q, want none", out.String())
	}
}

func TestParse_RejectsMalformedScripts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"unknown op", "frobnicate hard"},
		{"await-line with argument", "await-line now"},
		{"sleep without duration", "sleep soon"},
		{"exit without code", "exit loudly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.script)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", tc.script)
			}
		})
	}
}

func TestParse_ReportsLineNumbers(t *testing.T) {
	_, err := Parse(strings.NewReader("print ok\n\n# comment\nbogus"))
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Errorf("error %v does not name line 4", err)
	}
}

// lineReader reads newline-terminated lines from a pipe for assertions.
// Reads block on the pipe, so no polling; the deadline guards a wedged agent.
type lineReader struct {
	r   io.Reader
	buf []byte
}

func (l *lineReader) line(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if i := bytes.IndexByte(l.buf, '\n'); i >= 0 {
			line := string(l.buf[:i])
			l.buf = l.buf[i+1:]
			return line
		}
		if time.Now().After(deadline) {
			t.Fatalf("no line within deadline; buffered %q", l.buf)
		}
		chunk := make([]byte, 256)
		n, err := l.r.Read(chunk)
		if n > 0 {
			l.buf = append(l.buf, chunk[:n]...)
		}
		if err != nil && bytes.IndexByte(l.buf, '\n') < 0 {
			t.Fatalf("stream ended without a full line; buffered %q (err %v)", l.buf, err)
		}
	}
}
