# Status detection is layered: agent hooks first, screen heuristics as fallback

The spec proposed a single mechanism — regex over the output stream — to detect whether an Agent is Blocked. We rejected regex-over-raw-bytes entirely (escape sequences split patterns; ADR-0001 gives us a rendered grid to inspect instead) and made heuristics the *fallback*, not the primary: for agents that support it (Claude Code via its Notification/Stop hooks), Shevet installs hook configs that report structured status straight to the Server's unix socket. Screen Signals (output quiescence + prompt patterns near the cursor, per-agent profiles) cover everything else.

A future reader will see both hook plumbing and heuristic rules and may think one is vestigial — both are load-bearing: hooks give exactness for the primary use case, heuristics keep Shevet agent-agnostic.
