# Hook and Agent Signals reach the Server as gRPC calls made by the shevet CLI

The architecture used to say hook commands "ping the Server's unix socket" with a JSON payload. The listener on that socket is a gRPC server — HTTP/2 framing — so a raw JSON write is not merely unspecified, it cannot work. The Agent API PRD proposed the same JSON-writer shape and inherited the same problem. The alternatives were a second socket with its own ad-hoc protocol (framing, limits, request/response semantics all reinvented) or a protocol multiplexer on one socket (worse).

The decision exploits an invariant bootstrap already guarantees: **the shevet binary is present on every Host**. The hook command *is* the binary — `shevet signal --pane <id> --event <event> --reason <reason>` — which makes a unary gRPC call to a `SignalService` on the existing socket. The Agent API verbs (`shevet report`, `shevet ask`, `shevet done`) are siblings on the same service. One protocol end-to-end, versioned by the same additive-protobuf rule as everything else, and `shevet ask` gets its blocking request/response semantics as a plain unary call instead of a hand-rolled reply channel.

The spool guarantee (ADR-0005) is unchanged and lives where it always conceptually did — in the caller: when the CLI cannot reach the socket, it appends the event to `~/.shevet/signals.spool` per the spool protocol, and the Server drains on startup.

Rejected: a separate private JSON socket (second protocol surface to specify, secure, and version); a multiplexer sniffing JSON vs HTTP/2 on one socket (fragile, and gRPC owns the listener today for good reason).

## Consequences

- Hook configuration docs point at a CLI invocation, not a socket path — copy-pasteable and stable across protocol changes.
- Signal callers are version-skew-safe by construction: hooks run the same artifact bootstrap installed alongside the Server.
- `SignalService` is Host-local by intent; it rides the same socket, and SSH + socket mode 0600 remain the only trust boundary (ADR-0003).
