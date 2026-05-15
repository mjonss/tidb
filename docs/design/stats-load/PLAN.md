# Pin Down the Stats Sync-Load and Init-Stats Contract, Then Harden With Tests

This ExecPlan is a living document. Keep `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` up to date as work proceeds.

Reference: `PLANS.md` at repository root; this plan must be maintained according to it.

## Purpose / Big Picture

TiDB's statistics loading subsystem (sync load, init stats, and the cache barrier between them) has accumulated 17 documented bug fixes across roughly two years (see `notes/pr-cross-reference.md`). The retry policy has changed four times, the singleflight scope has been moved twice, and several non-obvious invariants have only integration-test coverage or none at all. A future refactor will be unsafe until the contract is written down and pinned by characterization tests.

After this work, a contributor can:

1. Read `docs/design/stats-load/CONTRACT.md` and learn, in one place, what guarantees `SendLoadRequests`, `SyncWaitStatsLoad`, `InitStats`, and `InitStatsLite` provide, with file:line citations.
2. Run a focused test set (`go test ./pkg/statistics/handle/syncload/... ./pkg/statistics/handle/...`) and see every contract clause exercised, including the fragile invariants currently uncovered.
3. Refactor any single component in this subsystem with confidence that the safety net will catch silent regressions.

In scope: sync load, init stats, the public `StatsCache` interface (Put/Drop/Update/WaitForAsyncUpdates/version observation), and the delta-collector freshness pipeline in `pkg/statistics/handle/usage/session_stats_collect.go` (DML counts to `mysql.stats_meta` version bump to cache invalidation to next read sees fresh stats).

Out of scope: autoanalyze scheduling, the ANALYZE worker (stats gathering itself), cache implementation internals (LFU/Ristretto admission, eviction policy, sharded keyset), and the `StatsHistory` subsystem (historic stats snapshots / time-travel feature embedded in `StatsHandle`). Internal cache mechanics are excluded; only the visible interface and ordering guarantees that callers depend on are in scope. Out of scope: actually performing the refactor; that becomes a separate ExecPlan informed by Phase 3's discrepancy log.

## Progress

- [x] (2026-05-14) Tools loaded; subsystem mapped via Explore agent.
- [x] (2026-05-14) Sync-load history file received from user at `sync-load-history.txt`.
- [x] (2026-05-14) All 17 historical PRs cross-referenced with current code; notes saved at `notes/pr-cross-reference.md`. Net status: 14 PRESENT, 3 REFACTORED, 0 MISSING.
- [x] (2026-05-14) ExecPlan drafted (this file).
- [x] (2026-05-14) Phase 0a complete: caller map produced at `CURRENT-USAGE.md`; raw per-directory notes at `notes/caller-map-*.md` (task #12). Coverage: 4 parallel agents over planner/core (17 files), planner/cardinality+property+implementation, executor (read paths), and external (ddl/domain/session/server/util/ttl/dxf/table/store).
- [x] (2026-05-15) Drafted `CONTRACT.md` sync-load section (task #2). 16 test obligations enumerated for Phase 1.
- [x] (2026-05-15) Drafted `CONTRACT.md` init-stats section (task #3). 15 test obligations.
- [x] (2026-05-15) Drafted `CONTRACT.md` pseudo-fallback section (task #4). 13 test obligations.
- [x] (2026-05-15) Drafted `CONTRACT.md` visible-StatsCache-interface section (task #10). 12 test obligations.
- [x] (2026-05-15) Drafted `CONTRACT.md` delta-collector freshness-pipeline section (task #11). 15 test obligations.
- [x] (2026-05-15) Inventoried existing tests (task #6). 68 tests mapped against 23 sync-load clauses; coverage matrix with 11 gaps highlighted. Saved at `notes/test-inventory.md`.
- [ ] Phase 1: characterization tests for each clause (task #7). Started 2026-05-15: 6 new tests added to `pkg/statistics/handle/syncload/stats_syncload_test.go` covering sync-load clauses 2, 5, 8, 12 (both paths), and 17. All pass under `-tags=intest`. Tests added: `TestSendLoadRequestsEmptyBatch`, `TestSyncWaitStatsLoadOnEmptyNeededItems`, `TestSyncWaitStatsLoadClearsNeededItemsOnSuccess`, `TestSyncWaitStatsLoadClearsNeededItemsOnTimeout`, `TestStatsLoadItemKeyMetaVsFullDistinct`, `TestGetSyncLoadConcurrencyByCPUBuckets`. Remaining sync-load gaps: 9, 14, 18, 19, 22, 23. Init/pseudo/cache/delta sections have not started.
- [ ] Phase 2: stress / fuzz / chaos tests with new failpoints (task #8).
- [ ] Phase 3: discrepancy log and bug filings (task #9).
- [ ] Phase 0c (deferred): ideal sketch after 0a+0b complete; no task yet.

## Surprises & Discoveries

- Observation: `RetryCount` has been edited four times in two years (`?` → 3 → 2 → 1) and the source comment hints the next step is `0`.
  Evidence: `notes/pr-cross-reference.md` PRs #52658, #57712, #60018; `pkg/statistics/handle/syncload/stats_syncload.go:48-52`.
  Implication: contract tests must reference `RetryCount` symbolically, not by literal value.

- Observation: Singleflight key shape encodes `(tableID, columnOrIndexID, isIndex, fullLoad)` and deliberately deduplicates meta-load and full-load separately.
  Evidence: `pkg/parser/model/model.go` `TableItemID.Key()` and `StatsLoadItem.Key()`.
  Implication: a test should pin this; future "optimization" that merges the two will silently degrade meta-load latency.

- Observation: Three fragile invariants currently have zero direct test coverage: worker retry-sleep distribution (#50956), per-task `HighPriority` set/reset (#51636), and the `errGetHistMeta` swallow-vs-propagate boundary (#56614).
  Evidence: `notes/pr-cross-reference.md` "Fragile invariants without direct tests".

- Observation: PR #52301 and PR #52830 each introduced symbols (`WorkingColMap`, the once-on-startup priority set) that were silently removed by later PRs.
  Evidence: `notes/pr-cross-reference.md` "Conflicting / superseded fixes".
  Implication: PR descriptions cannot be trusted as a source for the contract. Code is the only source; PR history is a starting point.

## Decision Log

- Decision: Scope is sync load + init stats. Autoanalyze and usage/delta-collector are excluded.
  Rationale: Historical bugs cluster at the sync/init seam (PR #54531 and #57803 most notably). Autoanalyze drives ANALYZE scheduling, not histogram loading, and is loosely related.
  Date/Author: 2026-05-14, mjonss + Claude.
  Superseded: see next entry.

- Decision: Scope widened to include the visible `StatsCache` interface (Put / Drop / Update / WaitForAsyncUpdates / CleanByPhysicalID / version observation) and the delta-collector freshness pipeline in `pkg/statistics/handle/usage/session_stats_collect.go`. Still out: autoanalyze scheduling, the ANALYZE worker, internal LFU/Ristretto mechanics.
  Rationale: The "DML to visible stats version" chain crosses contract boundaries the planner depends on. Pinning only sync/init leaves the cache-visibility seam (StatsCache.Update reading stats_meta versions written by DumpStatsDeltaToKV) unspecified, which is exactly where stale-read bugs hide. The cost is two extra Phase 0 sections; both are bounded to public-interface guarantees, not implementation.
  Date/Author: 2026-05-14, mjonss + Claude.

- Decision: Two artifact files. `CONTRACT.md` is the durable, code-adjacent statement of the contract. `PLAN.md` (this file) is the project plan and decays in relevance once Phase 3 completes.
  Rationale: PLANS.md format requires a living ExecPlan; AGENTS.md additionally rewards code-adjacent docs. Splitting them keeps each fit for its purpose.
  Date/Author: 2026-05-14, mjonss + Claude.

- Decision: No `doc.go` written until the contract is stable.
  Rationale: User feedback ("no speculative reservations") and AGENTS.md ("keep diffs minimal") both argue for waiting until we know the contract holds.
  Date/Author: 2026-05-14, mjonss + Claude.

- Decision: Phases 1-3 are sketched, not specified. Each phase will be expanded to milestone-grade detail only after the preceding phase finishes.
  Rationale: User feedback ("plan one unit at a time, don't pre-plan a series").
  Date/Author: 2026-05-14, mjonss + Claude.

- Decision: Phase 0 derives the contract from code only. The PR cross-reference notes are not a Phase 0 input.
  Rationale: The PR history was provided to show that this subsystem has had many issues (symptom evidence), not to dictate the contract. Letting symptoms drive the contract risks documenting fixes as features and missing untested invariants. The notes remain useful in Phase 1 as a regression-scenario catalog.
  Date/Author: 2026-05-14, mjonss + Claude.

- Decision: `StatsHistory` subsystem (historic stats snapshots, the `StatsHistory` interface embedded in `StatsHandle`) is out of scope. Keeping historic stats is not part of this work.
  Rationale: User-stated. Reduces investigation surface; historic snapshot semantics are largely independent of the live load path.
  Date/Author: 2026-05-14, mjonss + Claude.

- Decision: Phase 0 split into 0a (caller / needs map) and 0b (current implementation contract). Both run in parallel; both derive from current code only. Phase 0c (ideal sketch) is deferred and kept small until 0a and 0b are complete.
  Rationale: User-stated: "We cannot propose anything before we know more how it is actually used currently. So first document the current implementation." Mapping current use is the prerequisite for any future proposal of alternatives; documenting current implementation independently de-risks the ideal sketch when it happens. After 0a+0b, each contract clause will be tagged KEEP / TRANSFORM / CUT relative to the ideal so Phase 1 effort targets the right clauses.
  Date/Author: 2026-05-14, mjonss + Claude.

## Outcomes & Retrospective

Pending. To be filled in at the end of Phase 0 (contract docs complete) and again at the end of Phase 3.

## Context and Orientation

The stats loading subsystem lives in three subdirectories of `pkg/statistics/handle/`:

- `syncload/` (2 files): the synchronous load path triggered by the planner rule at `pkg/planner/core/rule/rule_collect_plan_stats.go`. Two worker channels (`neededItemsCh` for in-budget, `timeoutItemsCh` for over-budget), 5-10 workers sized by CPU, retries gated by `RetryCount`, deduplication via `singleflight.Group` keyed by `(tableID, id, isIndex, fullLoad)`.
- `initstats/` (2 files): startup-time bulk loaders. `RangeWorker` shards table IDs across goroutines (concurrency = `min(max(2, GOMAXPROCS-2), 16)` if `ForceInitStats`, else `GOMAXPROCS/2`). Paging size `initStatsStep=500` tables.
- `bootstrap.go` (in `handle/`): orchestrates `InitStats` (full) and `InitStatsLite` (lazy). Drives the cache update barrier `WaitForAsyncUpdates()` between phases.

The planner-visible interface is `StatsSyncLoad` declared at `pkg/statistics/handle/types/interfaces.go:443-459`. Its three methods:

- `SendLoadRequests(sc *stmtctx.StatementContext, neededHistItems []model.StatsLoadItem, timeout time.Duration) error` — enqueue with deadline; deduplicate; return per-item result channels in `sc.StatsLoad.ResultCh`.
- `SyncWaitStatsLoad(sc *stmtctx.StatementContext) error` — block on those channels until done, timed out, or all errored.
- `SubLoadWorker(exit chan, exitWg *util.WaitGroupEnhancedWrapper)` — spawn one worker goroutine.

Existing failpoints (sparse): `handleOneItemTaskPanic`, `mockReadStatsForOnePanic`, `mockReadStatsForOneFail` in syncload; `mockBucketsLoadMemoryLimit`, `beforeInitStats`, `beforeInitStatsLite` in handle/bootstrap. Phase 2 will likely add more.

Non-obvious terms:

- Lite init: server starts before all histograms are loaded; sync load lazily fills as queries arrive. Default in v8.4+. Implementation: `bootstrap.go:833`.
- Singleflight key: a string deduplicating concurrent requests that ask for the same statistic. Key format: `"<tableID>#<columnOrIndexID>#<isIndex>#<fullLoad>"`. Source: `pkg/parser/model/model.go`.
- Pseudo stats: a placeholder used by the planner when real stats are absent or stale. Returned by `GetPhysicalTableStats` when no entry exists.
- Empty column: a tombstone-like entry installed by sync load when a request lands on a column with no analyze data, preventing repeated re-requests. Implementation: `pkg/statistics/column.go:250-258` (`EmptyColumn`). Used by both sync load (PR #52427) and async load (PR #57723).

## Plan of Work

### Phase 0 (in progress): document the current state

Two parallel deliverables. Both derive from the current code only. No alternatives proposed yet.

**Phase 0a, caller / needs map (task #12).** Comprehensive mapping of every call site in the planner, optimizer, executor, session, domain, etc. that reads statistics. For each call site, record: file:line, which stat is read (count, modify_count, NDV, histogram, TopN, null count, index stats, version, last-analyze-version), which planning stage reads it, what staleness it tolerates, what fallback applies. Output: `docs/design/stats-load/CURRENT-USAGE.md`. Thoroughness is the point; an incomplete map means incomplete confidence later.

**Phase 0b, current implementation contract.** Produce `docs/design/stats-load/CONTRACT.md` with five sections and a file:line citation per clause:

1. Sync load (task #2).
2. Init stats (task #3).
3. Pseudo-stats and EmptyColumn fallback (task #4).
4. Visible `StatsCache` interface: what Put / Drop / Update / WaitForAsyncUpdates guarantee, version observation semantics (task #10).
5. Delta-collector freshness pipeline: DML to `stats_meta` version to cache invalidation, including the bound on staleness a caller can rely on (task #11).

Inventory existing tests in parallel (task #6). No code changes.

**Phase 0c (deferred), ideal sketch.** After 0a and 0b are complete, sketch what the optimizer/planner could make the best of given ideal statistics, not limited to the current implementation. Kept small for now; no task created yet. Output will be a `IDEAL.md` of two or three pages, and a KEEP / TRANSFORM / CUT tag added to each clause in `CONTRACT.md`.

### Phase 1 (pending): characterization tests

For every clause in `CONTRACT.md` not covered by an existing test, add a unit test in `pkg/statistics/handle/syncload/stats_syncload_test.go` or a test file under `pkg/statistics/handle/`. Phase 1's milestone is "every contract clause has a test that fails when the clause is violated." Where existing failpoints are insufficient, list the new failpoints required but do not add them yet (that is Phase 2).

### Phase 2 (pending): stress, fuzz, chaos

Add the failpoints identified in Phase 1. Add property tests over input parameter space (table count × column count × timeout distribution × concurrency × init-mode). Replay each of the 17 historical bug fixes as a chaos scenario to make sure it stays fixed.

### Phase 3 (pending): discrepancy log

Where Phase 1 or Phase 2 tests disagree with `CONTRACT.md`, decide per-case: update doc, file bug, or fix code immediately. Phase 3 output is a short `DISCREPANCIES.md` plus any bug filings in `pingcap/tidb`.

## Concrete Steps

Phase 0a (current, parallel with 0b):

    # Map all stats callers. Search comprehensively.
    grep -rn "GetPhysicalTableStats\|SendLoadRequests\|SyncWaitStatsLoad" pkg/
    grep -rn "ColumnIsLoadNeeded\|IndexIsLoadNeeded\|StatsLoadItem" pkg/
    grep -rn "TableInfoStats\|ColumnStats\|IndexStats" pkg/planner pkg/executor pkg/session

    # For each unique call site, record file:line, stat type, planning stage,
    # staleness tolerance, fallback on absent stats. Cover interface callers
    # and generated code.

    # Produce docs/design/stats-load/CURRENT-USAGE.md.

Phase 0b (current, parallel with 0a):

    # From repo root
    # Read the current sync-load implementation:
    Read pkg/statistics/handle/syncload/stats_syncload.go
    Read pkg/statistics/handle/syncload/stats_syncload_test.go
    Read pkg/statistics/handle/bootstrap.go (sections 800-960)
    Read pkg/statistics/handle/initstats/load_stats.go
    Read pkg/statistics/handle/initstats/load_stats_page.go

    # Read the visible cache interface and delta-collector:
    Read pkg/statistics/handle/types/interfaces.go (StatsCache section)
    Read pkg/statistics/handle/cache/statscache.go
    Read pkg/statistics/handle/usage/session_stats_collect.go

    # Produce docs/design/stats-load/CONTRACT.md with five sections.
    # Each clause cites file:line in this worktree. PR history is not a
    # Phase 0 input; it is only useful in Phase 1 (symptom catalog).

Phase 1 will provide its own command list once expanded.

## Validation and Acceptance

Phase 0 acceptance: `CONTRACT.md` exists, every clause cites a file:line, every clause cross-references the PR(s) that established it. Inventory entry exists for every existing test in `pkg/statistics/handle/syncload/*_test.go`.

Phase 1 acceptance: every clause in `CONTRACT.md` is annotated with one or more test names from `pkg/statistics/handle/syncload/` or `pkg/statistics/handle/`. `go test ./pkg/statistics/handle/syncload/...` passes locally.

Phase 2 acceptance: a `make failpoint-enable` + `go test ./pkg/statistics/handle/syncload/... -count=10 -race` run completes with no flakes; each historical PR has at least one regression scenario.

Phase 3 acceptance: `DISCREPANCIES.md` exists. Every entry is either "doc updated", "code fixed in PR #N", or "issue filed #N".

End-to-end: a hostile reviewer can take any one of the 17 historical PRs, revert its substantive change locally, and a test in this package fails. This is the operational definition of "the contract is pinned."

## Idempotence and Recovery

All Phase 0 work is doc-only and idempotent. Phase 1 and 2 add tests and (in Phase 2) failpoints; failpoints follow the AGENTS.md rule of `make failpoint-enable` before, `make failpoint-disable` after.

If a phase produces tests that fail on master (i.e., the contract is wrong, not the code), the recovery path is to update `CONTRACT.md` and re-derive the test. Record the iteration in `Surprises & Discoveries`.

## Artifacts and Notes

Cross-reference notes: `docs/design/stats-load/notes/pr-cross-reference.md`. **Not a Phase 0 input.** Useful in Phase 1 as a symptom catalog: each of the 17 historical bug fixes is a candidate regression scenario whose test we should make sure exists or add.

Source history file (user-provided): `sync-load-history.txt` at worktree root. Not committed; treat as private input.

## Interfaces and Dependencies

The contract docs describe but do not change:

- `pkg/statistics/handle/types/interfaces.go` `StatsSyncLoad`:
  - `SendLoadRequests(sc *stmtctx.StatementContext, neededHistItems []model.StatsLoadItem, timeout time.Duration) error`
  - `SyncWaitStatsLoad(sc *stmtctx.StatementContext) error`
  - `SubLoadWorker(exit chan, exitWg *util.WaitGroupEnhancedWrapper)`
- `pkg/statistics/handle/bootstrap.go`:
  - `(*Handle).InitStats(ctx context.Context, tableIDs ...int64) error`
  - `(*Handle).InitStatsLite(ctx context.Context, tableIDs ...int64) error`
- `pkg/statistics/handle/types.StatsCache` (visible interface, full scope):
  - `Get(tableID int64) (*statistics.Table, bool)`
  - `Put(tableID int64, t *statistics.Table)`
  - `Update(ctx context.Context, is infoschema.InfoSchema) error`
  - `UpdateStatsCache(u CacheUpdate)`
  - `WaitForAsyncUpdates()` (the barrier between init phases)
  - `CleanByPhysicalID(physicalID int64)`
- `pkg/statistics/handle/usage`:
  - `(*SessionStatsList).DumpStatsDeltaToKV(ctx, sctx, dumpAll bool, tableIDs ...int64)`
  - `SessionStatsItem` accumulator interface
  - The dump trigger conditions in `needDumpStatsDelta`

Tests will depend on the failpoint registry under `pkg/statistics/handle/syncload/` and may add new failpoints in Phase 2 with names beginning `mockSync*` for consistency with existing ones.
