# Task 7 — Atomic repository generation builder report

Date: 2026-08-28

## Scope and revision

- Plan: `docs/plans/2026-08-27-provider-agnostic-embeddings-vector-search.md`, Task 7.
- Brief: `.superpowers/sdd/2026-08-27-provider-agnostic-embeddings-vector-search/task-7-brief.md`.
- Base: `d58e253b88944accc18a99a9f1d127143397c826`.
- Verified worktree HEAD before the focused task commit: `d58e253b88944accc18a99a9f1d127143397c826`.
- Result commit: the focused commit containing this report; its exact hash is recorded in the task handoff because a commit cannot embed its own hash.
- Created: `internal/index/builder.go`, `internal/index/builder_test.go`, and this report.
- Preserved: CLI, concrete provider adapter, vector/hybrid search, module dependencies, and `graphify-out/`.

## TDD evidence

The public seams under test were the approved `NewBuilder` and `(*Builder).Replace` API, with fakes only at the external `embedding.Embedder` and `index.Store` boundaries.

1. Initial red: `go test ./internal/index -run 'TestBuilder|TestNewBuilder' -v` failed to compile because `NewBuilder`, `BuilderConfig`, `Report`, and related API did not exist.
2. First green: the off-mode/configuration slice passed 14 cases.
3. Semantic red: `go test ./internal/index -run 'TestBuilderSemantic' -v` failed 9 cases because the lexical-only implementation did not embed, classify failures, or publish semantic metadata.
4. Semantic green: the same 9 cases passed after the minimal semantic orchestration was added.
5. Final focused gate: `go test ./internal/index -run TestBuilder -v` passed 79 cases.

Coverage includes off/preferred/required policy, exact positional vector association, malformed batch tables in both enabled modes, deterministic identity separation, default and configured cost boundaries, pre-I/O failures, active/publication error categories, committed cleanup outcomes, mutation resistance, cancellation checkpoints, and concurrent calls.

## Verification evidence

All commands were run from the repository root without external services:

| Gate | Result |
| --- | --- |
| `go test ./internal/index -run TestBuilder -v` | PASS — 79 cases |
| `go test -race ./internal/index ./internal/index/sqlite` | PASS — 225 cases |
| `GOTOOLCHAIN=go1.24.0 CGO_ENABLED=0 go test ./...` | PASS — all 15 packages |
| `GOTOOLCHAIN=go1.24.0 CGO_ENABLED=0 go build ./...` | PASS |
| `GOTOOLCHAIN=go1.24.0 CGO_ENABLED=0 go vet ./...` | PASS |
| `GOTOOLCHAIN=go1.24.0 CGO_ENABLED=0 go version` | `go version go1.24.0 darwin/arm64` |
| `go mod verify` | PASS — all modules verified |
| `gofmt -d internal/index/builder.go internal/index/builder_test.go` | PASS — empty diff |
| `git diff --check` | PASS |

## Self-review

### Standards and architecture

No findings.

- Dependency direction remains provider-neutral: `internal/index` imports only the standard library plus `internal/embedding` and `internal/source`.
- No concrete adapter, CLI, transport, external database, time, randomness, or logging entered the module.
- The builder owns full-generation orchestration while `Store.Replace` retains atomic publication and optimistic concurrency ownership.
- Canonical `source.Corpus` chunks and references cross the seam unchanged, using defensive slice copies.
- Context cancellation and sanitized error categories are preserved without returning raw provider/store causes.
- No speculative incremental reuse or batching was introduced.

### Task 7 specification

No findings.

- Safe defaults are 20,000 chunks and 64 MiB of chunk text; limit failure occurs before active-generation lookup, embedding, or publication and uses overflow-safe subtraction.
- Off mode permits a nil embedder, makes no provider call, and publishes a cosine lexical-only generation.
- Enabled modes read the active base first, embed all chunk texts exactly once in canonical order, call `embedding.ValidateBatch`, require exact profile equality, and deep-copy vector slices.
- Only `errors.Is(err, embedding.ErrSemanticUnavailable)` degrades in preferred mode; required mode preserves that category, while parent cancellation, malformed output, and unknown provider failures remain fatal.
- Generation IDs use length-framed SHA-256 over version, scanner policy, corpus revision, and either the profile fingerprint or literal lexical-only identity.
- Publication occurs once. `ErrConcurrentIndex` is never retried, and committed cleanup errors return both the published report and typed committed outcome.

## Residual risks and explicit non-goals

- `embedding.Batch` has no per-vector IDs, so order integrity is necessarily positional. The builder validates quantity/shape and preserves returned order; the adapter contract remains responsible for mapping provider response indices into input order.
- M2 intentionally holds the complete text list and returned vectors in memory and re-embeds every chunk. HTTP batching remains adapter-owned; incremental reuse is outside Task 7.
- SQLite integration is exercised indirectly through its existing generation contracts and directly by the race/full-suite gates; Task 7 adds no new end-to-end CLI wiring.

No network request, external service, or professional repository was accessed. The existing local `graphify-out/` graph was queried read-only for orientation and was not modified.
