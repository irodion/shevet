package sshx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config is the effective SSH connection for one Host: the values OpenSSH
// would use to reach it, resolved from the developer's ~/.ssh/config, its
// Includes, Match blocks, and built-in defaults.
//
// It is populated by ResolveConfig, which shells out to `ssh -G` rather than
// re-parsing ssh_config. OpenSSH's config grammar (Include, Match, token
// expansion, per-option first-match-wins) is large and subtly versioned;
// `ssh -G` is the one parser guaranteed to agree with the ssh the developer
// already trusts (ADR-0003). We connect with golang.org/x/crypto/ssh because
// the primary transport needs the direct-streamlocal channel, which the ssh
// CLI does not expose — but we take the *configuration* from ssh itself.
type Config struct {
	// Host is the alias the user asked for (the connect argument), kept for
	// diagnostics and known_hosts lookups.
	Host string

	// HostName is the address to dial (config may rewrite Host → HostName).
	HostName string

	// Port is the TCP port of the remote sshd.
	Port string

	// User is the login user.
	User string

	// IdentityFiles are candidate private key paths, in config order, with
	// leading ~ expanded. Missing files are tolerated: a key may live only in
	// the agent. Encrypted keys are usable only through the agent.
	IdentityFiles []string

	// IdentitiesOnly, when true, forbids offering agent keys that are not also
	// listed in IdentityFiles (OpenSSH's IdentitiesOnly=yes).
	IdentitiesOnly bool

	// KnownHostsFiles are the user and global known_hosts files, in the order
	// ssh would consult them, with ~ expanded. Non-existent files are dropped.
	KnownHostsFiles []string

	// StrictHostKeyChecking mirrors the option of the same name: "yes",
	// "accept-new", "ask" (all treated as strict here), or "no"/"off" to
	// disable verification entirely.
	StrictHostKeyChecking string
}

// strict reports whether an unknown or changed host key must be a hard error.
// Only an explicit "no"/"off" opts out; every other value — including the
// interactive "ask"/"accept-new", which we cannot prompt for — is treated as
// strict, honoring the acceptance rule that an unknown key is an error, not a
// shrug (issue #10).
func (c *Config) strict() bool {
	switch strings.ToLower(c.StrictHostKeyChecking) {
	case "no", "off":
		return false
	default:
		return true
	}
}

// ResolveOptions tunes how ResolveConfig invokes ssh.
type ResolveOptions struct {
	// ConfigFile, when set, is passed to ssh as -F: an explicit ssh_config
	// instead of the user's default ~/.ssh/config. Empty uses the default,
	// which is the normal case.
	ConfigFile string
}

// ResolveConfig asks the local ssh client for the effective options for host,
// exactly as `ssh <host>` would compute them. host is an ssh_config alias or
// a plain hostname; anything ssh understands works.
func ResolveConfig(ctx context.Context, host string, opts ResolveOptions) (*Config, error) {
	if host == "" {
		return nil, fmt.Errorf("sshx: host must not be empty")
	}

	args := []string{"-G"}
	if opts.ConfigFile != "" {
		args = append(args, "-F", opts.ConfigFile)
	}
	args = append(args, host)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sshx: resolve ssh config for %q via `ssh -G`: %w: %s",
			host, err, strings.TrimSpace(stderr.String()))
	}

	return parseConfig(host, stdout.Bytes())
}

// parseConfig turns `ssh -G` output into a Config. The output is one
// "keyword value" per line, keywords lowercased; multi-valued keywords
// (identityfile) repeat across lines, while file lists (knownhostsfile) are
// space-separated on a single line.
func parseConfig(host string, out []byte) (*Config, error) {
	cfg := &Config{Host: host}

	scan := bufio.NewScanner(bytes.NewReader(out))
	for scan.Scan() {
		key, val, ok := strings.Cut(scan.Text(), " ")
		if !ok {
			continue // keyword with no value (e.g. a bare token) — ignore
		}
		val = strings.TrimSpace(val)
		switch key {
		case "hostname":
			cfg.HostName = val
		case "port":
			cfg.Port = val
		case "user":
			cfg.User = val
		case "identityfile":
			cfg.IdentityFiles = append(cfg.IdentityFiles, expandTilde(val))
		case "identitiesonly":
			cfg.IdentitiesOnly = val == "yes"
		case "userknownhostsfile", "globalknownhostsfile":
			for _, f := range strings.Fields(val) {
				cfg.KnownHostsFiles = append(cfg.KnownHostsFiles, expandTilde(f))
			}
		case "stricthostkeychecking":
			cfg.StrictHostKeyChecking = val
		}
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("sshx: read `ssh -G` output: %w", err)
	}

	if cfg.HostName == "" {
		cfg.HostName = host
	}
	if cfg.Port == "" {
		cfg.Port = "22"
	}
	return cfg, nil
}

// addr is the "host:port" endpoint to dial.
func (c *Config) addr() string {
	return c.HostName + ":" + c.Port
}

// expandTilde rewrites a leading ~ or ~/ to the current user's home. ssh -G
// leaves IdentityFile as "~/.ssh/id_ed25519"; golang.org/x/crypto/ssh and
// os.ReadFile need a real path. A bare "~user" form is left untouched — we do
// not resolve other users' homes.
func expandTilde(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}
