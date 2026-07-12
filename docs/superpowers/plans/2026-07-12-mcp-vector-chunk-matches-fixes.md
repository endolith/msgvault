# MCP Vector Chunk Matches Fixes Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make daemon-backed MCP semantic search return honest, bounded chunk matches with page-aware opt-in enrichment.

**Architecture:** Put chunk-to-match conversion in a shared `internal/vector/chunkmatch` package used by both API and MCP. Extend vector/hybrid HTTP search with offset, `has_more`, and opt-in match enrichment, then carry the additive wire fields through the generated client, daemon adapter, and MCP response. Keep raw-body locations optional pointer fields and document that omission is expected after preprocessing.

**Tech Stack:** Go, testify, Huma/OpenAPI, generated Go client, sqlite-vec/pgvector interfaces, MCP.

---

### Task 1: Shared chunk match conversion

**Files:**
- Create: `internal/vector/chunkmatch/matches.go`
- Create: `internal/vector/chunkmatch/matches_test.go`
- Modify: `internal/mcp/matches.go`
- Modify: `internal/mcp/matches_test.go`

- [ ] Write failing testify tests proving raw offset zero survives through pointers, transformed text omits raw locations, subject chunks omit locations, exact unique body chunks expose locations, and `min_score` filters excerpts.
- [ ] Run `go test -tags "fts5 sqlite_vec" ./internal/vector/chunkmatch ./internal/mcp` and confirm the new tests fail for the missing shared converter/pointer contract.
- [ ] Implement the minimal shared converter. Re-run preprocessing with the generation config, slice stored rune offsets, cap snippets without splitting UTF-8, and locate the complete body-only chunk only when it occurs exactly once in raw body text.
- [ ] Change MCP `messageMatch.CharOffset` and `Line` to `*int` with `omitempty`; always populate them for keyword matches and preserve nil for unlocatable vector matches.
- [ ] Re-run the targeted tests and keep them green.

### Task 2: Page-aware opt-in API enrichment

**Files:**
- Modify: `internal/api/handlers.go`
- Modify: `internal/api/handlers_test.go`
- Modify: `internal/api/routes.go`
- Modify: `internal/api/openapi.go`

- [ ] Write failing handler tests proving `include_matches=false` performs no body fetch/chunk scoring, `include_matches=true` enriches only the requested offset page, `min_score` filters only excerpts, and `has_more` comes from a probe result.
- [ ] Run the focused API tests and verify they fail because the query parameters and response fields do not exist.
- [ ] Parse vector/hybrid `offset`, `include_matches`, and `min_score`; fetch `offset + page_size + 1` ranked hits, slice the returned page before body hydration, and expose `has_more`.
- [ ] For opted-in pages, fetch each page body best-effort and use the shared converter with the engine query vector and active generation. Do not enrich skipped hits or the probe.
- [ ] Add Huma query parameter descriptions and bump the additive API schema minor version.
- [ ] Run API tests and confirm they pass.

### Task 3: Regenerate and adapt the daemon client

**Files:**
- Modify: `api/openapi.yaml` (generated)
- Modify: `pkg/client/openapi.yaml` (generated)
- Modify: `pkg/client/generated/*.go` (generated)
- Modify: `internal/daemonclient/cli.go`
- Modify: `internal/daemonclient/convert.go`
- Modify: `internal/daemonclient/store_adapter_test.go`

- [ ] Write failing daemon-client conversion tests for request offset/enrichment/min-score and response `has_more`/optional-location matches.
- [ ] Run the focused daemon-client tests and verify the new expectations fail.
- [ ] Add the additive API structs/query fields, run `make api-generate`, and adapt `CLIHybridSearchRequest`, `CLIHybridSearch`, and `CLIHybridSearchResult` conversions.
- [ ] Re-run daemon-client and OpenAPI checks.

### Task 4: Carry matches through production MCP

**Files:**
- Modify: `internal/mcp/handlers.go`
- Modify: `internal/mcp/server_test.go`
- Modify: `cmd/msgvault/cmd/mcp.go`
- Modify: `cmd/msgvault/cmd/mcp_test.go`

- [ ] Extend the daemon-path regression test to require match data and assert `Offset`, `IncludeMatches`, and `MinScore` are forwarded.
- [ ] Run the MCP and command tests and verify failure on the old daemon adapter.
- [ ] Extend MCP hybrid request/result types with offset, `has_more`, and matches. Stop client-side prefix slicing for daemon results; request the exact page with enrichment enabled and convert API matches into MCP matches.
- [ ] Keep the in-process path on the shared converter and preserve the existing message-pagination behavior.
- [ ] Re-run targeted MCP and command tests.

### Task 5: Correct public contracts

**Files:**
- Modify: `internal/mcp/server.go`
- Modify: `internal/mcp/server_test.go`
- Modify: `docs/usage/chat.md`

- [ ] Write/adjust schema tests for semantic subject-plus-body scope, excerpt-only `min_score`, optional vector locations, correct semantic tool guidance, and actual registration behavior.
- [ ] Run the schema tests and verify stale descriptions fail.
- [ ] Update tool descriptions and user documentation; restore `conversation_id` in the `list_messages` table.
- [ ] Re-run MCP tests and docs checks.

### Task 6: Commit, rebase, and verify

**Files:** all files above.

- [ ] Run `go fmt ./...` and `go vet -tags "fts5 sqlite_vec" ./...`.
- [ ] Run `make test`, `make lint-ci`, `make api-check`, and `git diff --check`.
- [ ] Review and scrub the complete public diff, then commit all implementation and generated changes with rationale-focused messages.
- [ ] Fetch and rebase onto `origin/main`; resolve conflicts without dropping mainline behavior.
- [ ] Re-run the full verification suite after the rebase.
- [ ] Push `HEAD` directly to the existing contributor PR branch and report the pushed commit plus verification results.
