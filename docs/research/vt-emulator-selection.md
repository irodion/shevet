# VT emulator library research (2026-07-06)

Resolves the spike from ARCHITECTURE.md §8. Method: three parallel literature/repo surveys plus a hands-on behavioral bench feeding identical byte sequences to each candidate and reading back the grid. Bench source and captured results are preserved in [`vtbench/`](./vtbench/) (note the two modules: `go-headless-term` cannot share a module with midterm — conflicting `go-ansicode` versions; `vtbench/vtbench2/` isolates it). Rerun with `go run .` (correctness) and `go run . perf` (throughput).

## Candidates

| | charm `x/vt` | `hinshun/vt10x` | `vito/midterm` | vaxis `widgets/term` | `go-headless-term` |
|---|---|---|---|---|---|
| Maintained (mid-2026) | ✅ active (pushed Jul 5 2026) | ❌ dead since 2023 | ✅ active (Dagger-driven) | ✅ active | ⚠️ new (Dec 2025), 1 maintainer |
| Feed raw bytes headless | ✅ `io.Writer` | ✅ | ✅ | ❌ **no released API** — v0.16.0 `WriteString` skips escape parsing; parser is PTY-coupled (`Start(cmd)`) | ✅ |
| Cell grid read | ✅ `CellAt` (grapheme+width+style) | ✅ `Cell` (rune only) | ✅ `Content`/`Format` regions | Snapshot only | ✅ `Cell(row,col)` |
| Damage API | ✅ typed damage + `Touched()` (Mar 2026) | ❌ coarse screen flag | ✅ per-row `Changes` counters | ❌ internal dirty flag | ✅ `DirtyCells()` |
| Scrollback | ✅ built-in, bounded (10k default) | ❌ none | ⚠️ `OnScrollback` hook (bring your own buffer) | internal only | ✅ pluggable provider |
| **Wide chars (bench)** | ✅ `你`+spacer, ZWJ emoji = 1 cell | ❌ **column drift** | ❌ **column drift** | ✅ (unreachable headless) | ✅ spacer correct; ZWJ cluster split |
| Alt screen (bench) | ✅ | ✅ | ✅ | ❌ headless | ✅ |
| DECSTBM scroll region (bench) | ✅ | ✅ | ✅ | ❌ headless | ❌ **bench failure**: region rows lost |
| SGR truecolor + 256 (bench) | ✅ | ✅ (packed uint32 quirks) | ✅ | — | ⚠️ truecolor ✅; 256-idx reads back black w/o palette resolve |
| Split escape across writes (bench) | ✅ | ✅ | ✅ | — | ✅ |
| Throughput (styled 80×24 stream) | 6.8 MB/s | 36 MB/s | 30 MB/s | — | not measured |
| Versioning | ⚠️ no tags, experimental repo | none | ✅ semver | ✅ semver | ✅ semver |
| License | MIT | MIT | MIT | Apache-2.0 | MIT |
| Production users | CodeCrafters testers, bubbleterm | expect-tests (survey, coder) | **Dagger TUI** | **aerc** (via PTY path) | none known |

## Key findings

1. **Wide chars are the deciding axis.** Shevet's server grid must agree with tmux's grid column-for-column (input coordinates, cursor position, status heuristics all depend on it). vt10x and midterm place the next glyph in the column *inside* a CJK char's second cell — permanent drift versus tmux. Only charm `x/vt` (and go-headless-term, partially) get this right, including ZWJ emoji clusters.
2. **vaxis's term widget cannot be fed bytes in any released version** — its escape parser only runs behind a real PTY started via `Start(cmd)`. Disqualified despite aerc pedigree.
3. **go-headless-term is the best API shape on paper** (`DirtyCells()`, pluggable scrollback) but failed the DECSTBM bench, misreports indexed colors via the naive read path, and is a 6-month-old single-maintainer project. Ecosystem landmine found: its `go-ansicode` dependency broke the `Handler` interface within v1.0.x patches, making it **impossible to import alongside midterm in one module**.
4. **charm x/vt is ~5× slower** than the st-lineage parsers (grapheme clustering cost; unrelated to scrollback — disabling it changed nothing). At 6.8 MB/s per pane this is still ~2 orders of magnitude above realistic agent output; not a blocker.
5. charm x/vt risks: untagged experimental module (pin a commit), open data race on `Close()` (issue charmbracelet/x#879 — serialize lifecycle ourselves; single-goroutine-per-pane makes this moot), damage/scrollback APIs are only months old.

## Recommendation

**charm `x/vt` as the emulator, wrapped behind a small internal `Emulator` interface** (`Write`, `CellAt`, `Cursor`, `Damage`, `Resize`, `Scrollback`) so it stays swappable; pin an exact commit. Fallback if it disappoints: midterm + an upstream wide-char patch (its maintainer merges external PRs quickly; Dagger dependence keeps it alive) — not vt10x (dead), not vaxis (no headless feed), not go-headless-term (correctness + maturity).
