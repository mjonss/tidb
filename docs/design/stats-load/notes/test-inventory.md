# Existing Tests for the Stats-Load Contract

Phase 0b deliverable (task 6). Inventory of existing tests that exercise any clause of the stats-load contract, with a coverage matrix mapped against the sync-load section's 23 numbered clauses (see `CONTRACT.md`). Source: structured caller-side investigation; agent run on 2026-05-15.

## Summary

68 distinct top-level `Test...` functions touch the stats-load contract directly or indirectly. Heaviest existing coverage: init-stats and JSON round-trip. Lightest: cache-mutex serialization, two-channel demotion order, and plan-cache skip on sync-load failure. The integration tests under `tests/integrationtest/t/statistics/` cover analyze, lock, and GC but not the sync-load contract itself.

## By package / file

### pkg/statistics/handle/syncload/

- `pkg/statistics/handle/syncload/stats_syncload_test.go:36` `TestConcurrentLoadHist`
  Exercises: end-to-end happy-path sync load of all columns for an analyzed table; full-load histograms/TopN present after.
  Contract clauses: 1, 3, 7, 9, 10, 12, 19, 20 (indirect)
  Failpoints: none
  Notes: `SetLease(1)`, `mathutil.MaxInt` timeout.

- `pkg/statistics/handle/syncload/stats_syncload_test.go:78` `TestConcurrentLoadHistTimeout`
  Exercises: `timeout=0` demotes every request to `timeoutItemsCh`; `SyncWaitStatsLoad` returns error; column stats not promoted.
  Contract clauses: 11, 16, 22 (partial), 12
  Failpoints: none
  Notes: Has commented-out TODOs at lines 102-104, 120.

- `pkg/statistics/handle/syncload/stats_syncload_test.go:125` `TestConcurrentLoadHistWithPanicAndFail`
  Exercises: panic recovery, error injection, singleflight dedup across two stmtCtx, retry path.
  Contract clauses: 4, 6, 13, 15, 19, 21 (indirect)
  Failpoints: `mockReadStatsForOnePanic`, `mockReadStatsForOneFail`
  Notes: Drives `HandleOneTask` directly with `StatsLoadConcurrency=-1`.

- `pkg/statistics/handle/syncload/stats_syncload_test.go:240` `TestRetry`
  Exercises: retry budget; after `RetryCount` retries the task errors out.
  Contract clauses: 13, 14 (indirect), 6
  Failpoints: `mockReadStatsForOneFail`

- `pkg/statistics/handle/syncload/stats_syncload_test.go:331` `TestSendLoadRequestsWaitTooLong`
  Exercises: no workers, queue blocks longer than per-send timeout (100ns); SendLoadRequests returns nil but each result delivers an error.
  Contract clauses: 6 (queue-full sub-outcome), 7, 10, 12

- `pkg/statistics/handle/syncload/stats_syncload_test.go:375` `TestSyncLoadOnObjectWhichCanNotFoundInStorage`
  Exercises: SQL-driven sync-load for a column with no stats_histograms row; transitions to loadNeeded=false without error.
  Contract clauses: 20, 1, 19

### pkg/statistics/handle/ (bootstrap_test.go + handletest/statstest/)

- `pkg/statistics/handle/bootstrap_test.go:23,35,48,63,69` `TestGenInitStatsHistogramsSQL*`, `TestGenInitStatsMetaSQL*`
  Exercises: pure SQL string assertions for the init queries.
  Contract clauses: InitStats branches (SQL form), paging

- `pkg/statistics/handle/handletest/statstest/stats_test.go:171,203,251` `TestStatsCacheProcess`, `TestStatsCache`, `TestStatsCacheMemTracker`
  Exercises: pseudo->analyzed transition, DDL interaction, memory tracking.
  Contract clauses: Pseudo flag, Update version-driven, MaxTableStatsVersion, Memory accounting

- `pkg/statistics/handle/handletest/statstest/stats_test.go:307` `TestStatsStoreAndLoad`
  Exercises: analyze, Clear, Update reload round-trip.
  Contract clauses: Get/Put visibility, InitStats vs Update

- `pkg/statistics/handle/handletest/statstest/stats_test.go:371,377,383,389` `TestInitStatsMemTrace*`
  Exercises: `MemConsumed()` after InitStats equals sum of per-table MemoryUsage, across Lite/non-Lite x concurrent/non-concurrent matrix.
  Contract clauses: InitStats branches, MemQuota tracking

- `pkg/statistics/handle/handletest/statstest/stats_test.go:404,474` `TestInitStats`, `TestInitStatsForPartitionedTable`
  Exercises: full InitStats; existence map, evicted column shape, index TopN+histogram.
  Contract clauses: InitStats branches, paging, EmptyColumn, Pseudo flag

- `pkg/statistics/handle/handletest/statstest/stats_test.go:617` `TestInitStatsWithoutHandlingDDLEvent`
  Exercises: stats_meta exists but no histogram rows (DDL event not handled); InitStats yields non-pseudo, non-analyzed with empty existence map.
  Contract clauses: InitStats branches, Pseudo vs IsStatsInitialized
  Notes: Comment "this test is incomplete because we should figure out what is the real impact to sync load and async load in this scenario."

- `pkg/statistics/handle/handletest/statstest/stats_test.go:654,663,740` `TestInitStatsVer2`, `TestInitStats51358`, `TestInitStatsIssue41938`
  Exercises: non-lite InitStats; nil-cache failpoint; regression for analyze with TopN=0.
  Contract clauses: InitStats branches, EmptyColumn
  Failpoints: `StatsCacheGetNil` (51358)

- `pkg/statistics/handle/handletest/statstest/stats_test.go:757` `TestDumpStatsDeltaInBatch`
  Exercises: `flush stats_delta *.*` writes both tables in one transaction at the same version.
  Contract clauses: DumpStatsDeltaToKV trigger, Version bump

- `pkg/statistics/handle/handletest/statstest/stats_test.go:785,819` `TestInitStatsForTableWithTopNButNoBuckets`, `TestInitStatsMemoryFullBlocksBucketsButKeepsTopN`
  Exercises: InitStats with TopN-no-buckets; memory pressure blocks buckets but keeps TopN.
  Contract clauses: InitStats branch, MemQuota during init
  Failpoints: `mockBucketsLoadMemoryLimit` (819)

- `pkg/statistics/handle/handletest/initstats/init_stats_test.go:73,130` `TestLiteInitStatsWithTableIDs`, `TestNonLiteInitStatsWithTableIDs`
  Exercises: per-table-ID InitStatsLite / InitStats variants; idempotent across multiple calls.
  Contract clauses: InitStats branches (targeted), Get/Put visibility

- `pkg/statistics/handle/handletest/initstats/init_stats_test.go:194,207` `TestConcurrentlyInitStatsWith[Without]MemoryLimit`
  Exercises: full InitStats under simulated full/not-full cache via `handle.IsFullCacheFunc` override.
  Contract clauses: InitStats, MemQuota, Concurrency, TriggerEvict
  Notes: Hard-codes expected IDs branching on `kerneltype.IsClassic`; fragile.

- `pkg/statistics/handle/handletest/initstats/init_stats_test.go:277,286` `TestDropTableBeforeConcurrentlyInitStats`, `TestDropTableBeforeNonLiteInitStats`
  Exercises: dropping a table before InitStats does not error.

- `pkg/statistics/handle/handletest/initstats/init_stats_test.go:312` `TestSkipStatsInitWithSkipInitStats`
  Exercises: `SkipInitStats=true`, after `<-h.InitStatsDone` cache does not contain analyzed table.
  Contract clauses: InitStatsDone signal, ForceInitStats (skip variant)

- `pkg/statistics/handle/handletest/initstats/init_stats_test.go:343` `TestNonLiteInitStatsAndCheckTheLastTableStats`
  Exercises: last-element edge of pagination boundary.
  Contract clauses: paging (RangeWorker), InitStats

### pkg/statistics/handle/cache/ (visible API)

- `pkg/statistics/handle/cache/statscache_test.go:27,76` `TestCacheOfBatchUpdate`, `TestUpdateStatsHealthyMetrics`
  Exercises: batch-update flush threshold; healthy-bucket gauge math.

- `pkg/statistics/handle/cache/internal/lfu/lfu_cache_test.go:32,49,83,95,117,142,172,241,271,305` `TestLFU*`
  Exercises: Put/Get/Del lifecycle, mem accounting, eviction, concurrency, capacity shrink.
  Contract clauses: Get/Put visibility, WaitForAsyncUpdates, TriggerEvict, MemQuota

- `pkg/statistics/handle/handletest/handle_test.go:572` `TestStatsCacheUpdateSkip`
  Exercises: Update without table change does not replace cached struct (identity preserved).

- `pkg/statistics/handle/handletest/handle_test.go:720` `TestEvictedColumnLoadedStatus`
  Marked `t.Skip("skip this test because it is useless")`. Eviction-path coverage gap.

- `pkg/statistics/handle/handletest/handle_test.go:751` `TestUninitializedStatsStatus`
  Exercises: non-analyzed table after Update has not-initialized columns; both branches of `tidb_enable_pseudo_for_outdated_stats` show `stats:pseudo`.
  Contract clauses: Pseudo flag vs StatsVersion (two coexisting signals)

- `pkg/statistics/handle/handletest/handle_test.go:1006` `TestStatsCacheUpdateTimeout`
  Failpoints: `util/ExecRowsTimeout`. Cached stats are not corrupted on Update failure.

- `pkg/statistics/handle/handletest/handle_test.go:1080,1100` `TestStatsCacheShouldNotCache*`
  Exercises: system tables and temporary tables stay out of cache.

- `pkg/statistics/handle/handletest/handle_test.go:833,924` `TestInitStatsLite`, `TestInitStatsLiteRecordsSynthesizedColumnStats`
  Exercises: full sync-load + async-load + cache flow on top of InitStatsLite; DDL-synthesized column metadata recorded.

- `pkg/statistics/handle/handletest/handle_test.go:1126,1225,1325` `TestPrunedIndexesNoAsyncStatsLoad*`
  Exercises: index pruning interacts with async load (`tidb_stats_load_sync_wait=0`); only kept indexes load.

### pkg/statistics/handle/usage/

- `pkg/statistics/handle/usage/collector/collector_test.go:25,45,74` `TestSession[Parallel]SendDelta[Sync]`
  Exercises: SessionStatsItem accumulator; per-session vs global merging.

- `pkg/statistics/handle/usage/session_stats_collect_test.go:33,62,98,167,274,310` `TestPredicateUsage_*`, `TestDumpColStatsUsageWriter_ConcurrentMultiTables`, `TestDumpStatsDelta*`
  Exercises: predicate-column usage, throttling, concurrent dump (skipped), InitTime persistence/merge.
  Contract clauses: SessionStatsItem accumulator, DumpStatsDeltaToKV trigger conditions, InitTime preservation

- `pkg/statistics/handle/usage/predicate_column_test.go:26,57,103,147,183,213,242` `TestCleanupPredicateColumns`, `TestAnalyze*`
  Exercises: predicate-column cleanup on drop; analyze flow with `tidb_analyze_column_options`.

- `pkg/statistics/handle/usage/index_usage_integration_test.go:29` `TestGCIndexUsage`
  Exercises: GC removes index-usage rows after DROP INDEX / DROP TABLE.

- `pkg/statistics/handle/usage/indexusage/collector_test.go:36,59,122,219` index-usage collector internals.

### pkg/statistics/handle/storage/

- `pkg/statistics/handle/storage/read_test.go:33,99,151,167` `TestLoadStats`, `TestLoadNonExistentIndexStats`, `TestColumnStatsIsInvalidSkipsInternalColumnID`, `TestLoadNeededHistogramsSkipsInternalColumnID`
  Exercises: full async pathway via `LoadNeededHistograms`; non-existent index resilience; internal column ID skip.
  Contract clauses: LoadNeededHistograms, EmptyColumn, errGetHistMeta swallow, GCStats-like cleanup

- `pkg/statistics/handle/storage/gc_test.go:30,63,102,123,137` `TestGCStats`, `TestGCPartition`, `TestGCColumnStatsUsage`, `TestDeleteAnalyzeJobs`, `TestExtremCaseOfGC`
  Exercises: GC removes rows after drop column/table/partition; analyze-jobs cleanup; edge case where stats_histograms empty.
  Failpoints: `injectGCStatsLastTSOffset` (137)
  Notes: `TestGCPartition` carries FIXME(#68076).

- `pkg/statistics/handle/storage/stats_read_writer_test.go:28,87,132,195` `TestUpdateStatsMetaVersionForGC`, `TestSlowStatsSaving*`, `TestFailedToHandleSlowStatsSaving`
  Failpoints: `slowStatsSaving`, `failToSaveStats`

- `pkg/statistics/handle/storage/dump_test.go:85,144,178,204,231,277,319,401,419,436,487,582,666`
  Exercises: DumpStatsToJSON + LoadStatsFromJSON round-trip; LastStatsHistVersion bump; partition load.

### pkg/planner/cardinality/ (pseudo)

- `pkg/planner/cardinality/selectivity_test.go:2182` `TestBuiltinInEstWithoutStats`
  Exercises: `IN (...)` selectivity falls back to pseudo (TableFullScan `stats:pseudo`); InitStatsLite and InitStats both leave existence map with `HasAnalyzed=false`.
  Contract clauses: PseudoTable construction, two pseudo signals
  Notes: Canonical pseudo-fallback test. No dedicated `TestPseudoTable` or `TestEmptyColumn` exists.

### pkg/executor/ (DROP STATS / LOAD STATS)

- `pkg/executor/test/simpletest/simple_test.go:530,598,653` `TestDropPartitionStats`, `TestDropStats`, `TestDropStatsForMultipleTable`
  Exercises: DROP STATS post-state (Pseudo=false, StatsVer=0, IsStatsInitialized=false); partition and multi-table variants.
  Contract clauses: DROP STATS soft delete, Pseudo flag vs StatsVersion

- No `load_stats_test.go` exists. LOAD STATS is exercised via JSON round-trip tests in storage/dump_test.go.

### tests/integrationtest/ (SQL-level)

- `tests/integrationtest/t/statistics/handle.test` and `integration.test` cover analyze, GC, lock indirectly; none of the sync-load contract clauses are pinned at the SQL surface here. `TestNotLoadedStatsOnAllNULLCol` (integration.test:10) is the closest: "stats on all-NULL column usable even when not loaded" -> touches clause 20 (EmptyColumn install) and pseudo-fallback indirectly.

## Coverage matrix (sync-load clauses 1-23)

| # | Clause | Existing tests |
|---|--------|---------------|
| 1 | SendLoadRequests pre-filters cached items | TestConcurrentLoadHist (indirect), TestInitStatsLite, TestSyncLoadOnObjectWhichCanNotFoundInStorage, TestPrunedIndexesNoAsyncStatsLoad* |
| 2 | Empty batch returns nil | **GAP** |
| 3 | Batch state on StatementContext | TestConcurrentLoadHist, TestConcurrentLoadHistTimeout, TestConcurrentLoadHistWithPanicAndFail |
| 4 | Singleflight dedup | TestConcurrentLoadHistWithPanicAndFail (two stmtCtxs share result) |
| 5 | Meta vs full dedup separation | **GAP** |
| 6 | Three terminal outcomes (delivered / timed-out / queue-full) | TestConcurrentLoadHist (delivered), TestConcurrentLoadHistTimeout (timed-out), TestSendLoadRequestsWaitTooLong (queue-full) |
| 7 | SendLoadRequests does not block | TestSendLoadRequestsWaitTooLong, TestConcurrentLoadHist |
| 8 | SyncWaitStatsLoad no-op on empty NeededItems | **GAP** |
| 9 | One timer per batch | **GAP** |
| 10 | SyncWaitStatsLoad per-item result handling | TestConcurrentLoadHist, TestConcurrentLoadHistTimeout, TestSendLoadRequestsWaitTooLong |
| 11 | SyncWaitStatsLoad batch timeout | TestConcurrentLoadHistTimeout, TestSendLoadRequestsWaitTooLong |
| 12 | Clears NeededItems on return | Soft GAP (implicit only) |
| 13 | RetryCount=1 budget | TestRetry |
| 14 | Randomized inter-retry sleep | **GAP** |
| 15 | Panic recovery | TestConcurrentLoadHistWithPanicAndFail |
| 16 | Two-channel demotion | TestConcurrentLoadHistTimeout (demotion only; no priority-order assertion) |
| 17 | Worker pool sizing by CPU | **GAP** |
| 18 | Per-task session + HighPriority | **GAP** |
| 19 | Cache update mutex serialization | **GAP** |
| 20 | EmptyColumn install for not-analyzed | TestSyncLoadOnObjectWhichCanNotFoundInStorage, TestLoadNonExistentIndexStats, internal-ID tests, TestInitStats |
| 21 | errGetHistMeta swallow | TestLoadNonExistentIndexStats |
| 22 | Pseudo-on-timeout pathway | Partial (TestConcurrentLoadHistTimeout returns error only; no toggle of `StatsLoadPseudoTimeout`) |
| 23 | Plan-cache skip on sync-load failure | **GAP** |

## Gaps / under-tested clauses

Top Phase 1 candidates:

- **2, 8**: Empty-batch invariants. Trivial unit tests.
- **5**: Meta vs full dedup separation. Concurrent meta and full requests for the same column should produce two tasks; current code makes them distinct singleflight keys.
- **9**: One timer per batch. Asserts a multi-item batch has a single deadline.
- **14**: Randomized inter-retry sleep. Clock injection or statistical test.
- **17**: Worker pool sizing by CPU. Pure unit; assert `GetSyncLoadConcurrencyByCPU` returns expected values.
- **18**: Per-task session + HighPriority. Tracing failpoint inside `handleOneItemTask`.
- **19**: Cache update mutex. Property test with many concurrent updates for the same table.
- **22, 23**: Pseudo-on-timeout + plan-cache skip. SQL-level test with forced timeout and `tidb_stats_load_pseudo_timeout=1`.
- **InitStatsDone listener gating** (server-side ForceInitStats): no test asserts the listener side respects the channel.

## Out of pattern

- `TestEvictedColumnLoadedStatus` (handle_test.go:720) marked `t.Skip("skip this test because it is useless")`. Eviction coverage opportunity.
- `TestDumpColStatsUsageWriter_ConcurrentMultiTables` (session_stats_collect_test.go:167) skipped with "run manually if needed".
- `TestGCPartition` (gc_test.go:63) has FIXME(#68076).
- `TestInitStatsWithoutHandlingDDLEvent` (stats_test.go:617) marked incomplete: "we should figure out what is the real impact to sync load and async load in this scenario."
- `TestConcurrentLoadHistTimeout` has commented-out TODOs at lines 102-104, 120.
- `TestConcurrentlyInitStatsWithMemoryLimit` hard-codes expected IDs branching on `kerneltype.IsClassic`.
- `TestRetry` deliberately resets `task1.Retry = 0` between iterations.
- `TestSyncLoadOnObjectWhichCanNotFoundInStorage` uses SQL-driven path with `Eventually` polling instead of direct API calls.
- Integration tests under `tests/integrationtest/t/statistics/` do not cover the sync-load contract at the SQL surface; Phase 1 may want a dedicated `.test/.result` pair for sync-load behavior under `tidb_stats_load_sync_wait` and `tidb_stats_load_pseudo_timeout`.
