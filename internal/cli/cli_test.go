package cli

import (
	"context"
	"os"
	"strings"
	"testing"
)

// run invokes the CLI with captured output.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRun_NoArgsPrintsUsage(t *testing.T) {
	code, _, stderr := run(t)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr does not contain usage:\n%s", stderr)
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "frobnicate")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("stderr does not name the unknown command:\n%s", stderr)
	}
}

func TestRun_HelpGoesToStdout(t *testing.T) {
	code, stdout, _ := run(t, "help")
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "serve") || !strings.Contains(stdout, "connect") {
		t.Errorf("help does not list core commands:\n%s", stdout)
	}
	if strings.Contains(stdout, "_proxy") || strings.Contains(stdout, "_agent") {
		t.Errorf("help leaks a hidden command:\n%s", stdout)
	}
}

func TestAgent_RequiresScript(t *testing.T) {
	code, _, stderr := run(t, "_agent")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--script is required") {
		t.Errorf("stderr does not explain the missing flag:\n%s", stderr)
	}
}

func TestAgent_RejectsMalformedScript(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bad.script"
	if err := os.WriteFile(path, []byte("frobnicate\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	code, _, stderr := run(t, "_agent", "--script", path)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown op") {
		t.Errorf("stderr does not name the parse error:\n%s", stderr)
	}
}

func TestRun_Version(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "shevet dev") {
		t.Errorf("version output unexpected:\n%s", stdout)
	}
}

func TestConnect_RequiresTarget(t *testing.T) {
	code, _, stderr := run(t, "connect")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--socket") {
		t.Errorf("stderr does not mention --socket:\n%s", stderr)
	}
}

func TestConnect_RemoteSocketRequiresHost(t *testing.T) {
	code, _, stderr := run(t, "connect", "-socket", "/tmp/x.sock", "-remote-socket", "/r.sock")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "remote-socket") {
		t.Errorf("stderr does not explain --remote-socket needs a host:\n%s", stderr)
	}
}

func TestConnect_RejectsHostPlusSocket(t *testing.T) {
	code, _, stderr := run(t, "connect", "-socket", "/tmp/x.sock", "somehost")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr does not explain exclusivity:\n%s", stderr)
	}
}

func TestServe_RejectsPositionalArgs(t *testing.T) {
	code, _, stderr := run(t, "serve", "extra")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("stderr does not flag the argument:\n%s", stderr)
	}
}

func TestProxy_MissingSocketIsActionable(t *testing.T) {
	// Pumping to a socket that isn't there fails with a message that names
	// the likely cause — the Client relays this over SSH.
	code, _, stderr := run(t, "_proxy", "/nonexistent/shevet/absent.sock")
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "Server running") {
		t.Errorf("stderr does not explain the missing Server:\n%s", stderr)
	}
}

func TestProxy_TooManyArgs(t *testing.T) {
	code, _, stderr := run(t, "_proxy", "/a.sock", "/b.sock")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "at most one") {
		t.Errorf("stderr does not flag the extra argument:\n%s", stderr)
	}
}
