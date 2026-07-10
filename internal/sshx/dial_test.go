package sshx

import (
	"context"
	"errors"
	"testing"
)

// TestDialRejectsProxyConfig asserts that a Host reachable only through a
// bastion or proxy command fails fast with ErrUnsupportedProxy rather than a
// direct dial to HostName:Port that would time out or be refused before auth.
func TestDialRejectsProxyConfig(t *testing.T) {
	for name, cfg := range map[string]*Config{
		"ProxyJump":    {Host: "prod", HostName: "10.0.0.5", Port: "22", ProxyJump: "bastion"},
		"ProxyCommand": {Host: "prod", HostName: "10.0.0.6", Port: "22", ProxyCommand: "/usr/bin/nc %h %p"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Dial(context.Background(), cfg)
			if !errors.Is(err, ErrUnsupportedProxy) {
				t.Fatalf("Dial error = %v, want ErrUnsupportedProxy", err)
			}
		})
	}
}
