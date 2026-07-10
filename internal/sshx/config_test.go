package sshx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseConfig(t *testing.T) {
	// Representative `ssh -G` output: repeated identityfile lines, a
	// space-separated known_hosts list, and a Host→HostName rewrite.
	out := `host prod
hostname 10.0.0.7
port 2200
user deploy
identitiesonly yes
stricthostkeychecking accept-new
identityfile ~/.ssh/id_ed25519
identityfile ~/.ssh/id_rsa
userknownhostsfile ~/.ssh/known_hosts ~/.ssh/known_hosts2
globalknownhostsfile /etc/ssh/ssh_known_hosts
`
	cfg, err := parseConfig("prod", []byte(out))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	home, _ := os.UserHomeDir()
	if cfg.HostName != "10.0.0.7" {
		t.Errorf("HostName = %q, want 10.0.0.7", cfg.HostName)
	}
	if cfg.Port != "2200" {
		t.Errorf("Port = %q, want 2200", cfg.Port)
	}
	if cfg.User != "deploy" {
		t.Errorf("User = %q, want deploy", cfg.User)
	}
	if !cfg.IdentitiesOnly {
		t.Error("IdentitiesOnly = false, want true")
	}
	wantIDs := []string{
		filepath.Join(home, ".ssh/id_ed25519"),
		filepath.Join(home, ".ssh/id_rsa"),
	}
	if !slices.Equal(cfg.IdentityFiles, wantIDs) {
		t.Errorf("IdentityFiles = %v, want %v", cfg.IdentityFiles, wantIDs)
	}
	wantKH := []string{
		filepath.Join(home, ".ssh/known_hosts"),
		filepath.Join(home, ".ssh/known_hosts2"),
		"/etc/ssh/ssh_known_hosts",
	}
	if !slices.Equal(cfg.KnownHostsFiles, wantKH) {
		t.Errorf("KnownHostsFiles = %v, want %v", cfg.KnownHostsFiles, wantKH)
	}
	if cfg.addr() != "10.0.0.7:2200" {
		t.Errorf("addr() = %q, want 10.0.0.7:2200", cfg.addr())
	}
}

func TestParseConfigDefaults(t *testing.T) {
	// A bare host with no hostname/port lines falls back to the alias and 22.
	cfg, err := parseConfig("example", []byte("user someone\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.HostName != "example" {
		t.Errorf("HostName = %q, want example", cfg.HostName)
	}
	if cfg.Port != "22" {
		t.Errorf("Port = %q, want 22", cfg.Port)
	}
}

func TestParseConfigProxy(t *testing.T) {
	// ProxyJump/ProxyCommand are captured so Dial can reject them; the "none"
	// sentinel that disables a proxy reads back as unset.
	jump, err := parseConfig("via", []byte("proxyjump bastion.example.com\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if jump.ProxyJump != "bastion.example.com" {
		t.Errorf("ProxyJump = %q, want bastion.example.com", jump.ProxyJump)
	}

	cmd, _ := parseConfig("via", []byte("proxycommand /usr/bin/nc %h %p\n"))
	if cmd.ProxyCommand != "/usr/bin/nc %h %p" {
		t.Errorf("ProxyCommand = %q, want the nc command", cmd.ProxyCommand)
	}

	none, _ := parseConfig("via", []byte("proxycommand none\nproxyjump none\n"))
	if none.ProxyCommand != "" || none.ProxyJump != "" {
		t.Errorf("proxy=none should read as unset, got ProxyCommand=%q ProxyJump=%q", none.ProxyCommand, none.ProxyJump)
	}
}

func TestConfigStrict(t *testing.T) {
	// Only explicit no/off opts out; everything else stays strict so an
	// unknown host key is an error, not a shrug (issue #10).
	for value, wantStrict := range map[string]bool{
		"yes":        true,
		"ask":        true,
		"accept-new": true,
		"":           true,
		"no":         false,
		"off":        false,
		"OFF":        false,
	} {
		c := &Config{StrictHostKeyChecking: value}
		if got := c.strict(); got != wantStrict {
			t.Errorf("strict(%q) = %v, want %v", value, got, wantStrict)
		}
	}
}
