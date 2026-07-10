# Bootstrap uploads only artifacts that match a sha256 manifest embedded in the Client

Bootstrap (ADR-0005) pushes a Server binary to the Host — code that will run on a machine the developer owns. The open question was how the Client decides the artifact it is about to upload (release download or `~/.shevet/artifacts` cache entry) is trustworthy.

The decision: every release build embeds the sha256 manifest of all its platform artifacts into the Client binary itself. Before upload, the Client hashes the artifact and requires a manifest match — the same check whether the bytes came from a fresh download or the local cache, so a poisoned cache and a tampered download fail identically. The chain of trust ends at the binary the developer already chose to execute, which is the right anchor for a single-developer tool: trusting the manifest is the same act as trusting the Client that enforces it.

Locally built artifacts (development, unreleased platforms) won't match any manifest by construction, so the escape hatch is explicit: an `--allow-unverified-artifact`-style flag, making the trust decision visible on the command line instead of silent.

Rejected: no verification / trust-on-first-use (a silently corrupted or tampered artifact gets executed on the Host); a signature scheme with key management (overkill — there is no third-party distribution channel to defend, and the verifier and the trust root would still be the same Client binary).

## Consequences

- The release process must produce the manifest before (or while) building the Client, making artifact hashing part of the build graph.
- A *verified* bootstrap can only install Servers of the Client's own release; that is already the upgrade model (connect re-bootstraps to the Client's version). The explicit-flag path is exactly the release from this rule, for local and unreleased artifacts.
