# Transport is gRPC over an SSH tunnel to a unix socket, not mTLS on a public port

The spec called for gRPC + mutual TLS. For our scope (one developer, Hosts they already own — see CONTEXT.md) mTLS means operating a miniature PKI (CA, per-host certs, distribution, rotation) to protect machines the developer can already reach over SSH. Instead the Server listens **only on a unix socket**; the Client dials the Host via SSH (golang.org/x/crypto/ssh + ssh-agent) and runs gRPC through the forwarded connection.

We keep gRPC/Protobuf as the protocol — bi-directional streams fit the render/input/telemetry channels — only the transport and auth layer changed.

Concrete mechanism: the primary path is OpenSSH's `direct-streamlocal@openssh.com` channel (`golang.org/x/crypto/ssh` → `Client.Dial("unix", socketPath)`), forwarding directly to the remote unix socket. For SSH servers without streamlocal forwarding, the fallback is `ssh exec` of `shevet _proxy` — a hidden subcommand of the shevet binary (already on the Host per ADR-0005) that pipes stdio to the unix socket. No third-party remote helpers (`socat`, `nc`) in either path.

## Consequences

- Hosts expose zero open TCP ports; auth and encryption ride existing SSH credentials.
- The same SSH connection can bootstrap the Host: upload the binary, (re)start the Server, health-check it.
- A direct mTLS TCP listener can be added later as an opt-in without touching the protocol, if a no-SSH environment ever appears.
- Connection latency includes SSH handshake on first dial; the spec's "sub-millisecond connection" claim was marketing and is dropped.
