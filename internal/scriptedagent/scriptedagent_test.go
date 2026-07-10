package scriptedagent

import (
	"bufio"
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

	out := bufio.NewScanner(outR)
	if got := nextLine(t, out); got != "before" {
		t.Fatalf("first line = %q, want %q", got, "before")
	}

	// The agent must now be parked on await-line: no further output may
	// exist before we provide input. Feed the line and expect the rest.
	if _, err := inW.Write([]byte("go\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if got := nextLine(t, out); got != "after" {
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

	// The code is meaningless on error (the caller owns failure exit codes);
	// only the error matters.
	if _, err := s.Run(inR, io.Discard); err == nil {
		t.Fatal("Run succeeded with closed stdin at await-line, want error")
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
		{"read-raw without path", "read-raw 12"},
		{"read-raw non-numeric count", "read-raw lots /tmp/x"},
		{"read-raw negative count", "read-raw -1 /tmp/x"},
		{"spam without text", "spam 100"},
		{"spam non-numeric count", "spam lots noise"},
		{"spam negative count", "spam -1 noise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.script)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", tc.script)
			}
		})
	}
}

func TestParse_ReadRaw(t *testing.T) {
	s := parse(t, "read-raw 42 /run/shevet/corpus.bin")
	if len(s.steps) != 1 {
		t.Fatalf("parsed %d steps, want 1", len(s.steps))
	}
	st := s.steps[0]
	if st.op != opReadRaw || st.count != 42 || st.path != "/run/shevet/corpus.bin" {
		t.Errorf("step = %+v, want read-raw count=42 path=/run/shevet/corpus.bin", st)
	}
}

func TestRun_SpamFloodsNumberedLines(t *testing.T) {
	s := parse(t, "spam 3 flood line")

	var out strings.Builder
	if _, err := s.Run(strings.NewReader(""), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "flood line 1\nflood line 2\nflood line 3\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestParse_ReportsLineNumbers(t *testing.T) {
	_, err := Parse(strings.NewReader("print ok\n\n# comment\nbogus"))
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Errorf("error %v does not name line 4", err)
	}
}

// nextLine reads the next newline-terminated line from the scanner; reads
// block on the pipe, so a misbehaving agent hangs into the go test timeout
// rather than being masked by a shorter local deadline.
func nextLine(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("stream ended without a full line (err %v)", sc.Err())
	}
	return sc.Text()
}
