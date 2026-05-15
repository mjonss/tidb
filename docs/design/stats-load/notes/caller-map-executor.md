# pkg/executor stats call sites

## Summary

The `pkg/executor` tree reads statistics in three broad shapes: (1) per-query index-usage reporting that pulls `RealtimeCount` and `PseudoVersion` from the per-statement `UsedStatsInfo` snapshot the planner builds; (2) administrative surfaces -- `SHOW STATS_*`, `SHOW INDEX`, and several `INFORMATION_SCHEMA` tables -- that pull from the in-memory stats handle (`GetPhysicalTableStats`, `GetNonPseudoPhysicalTableStats`, `GetLockedTables`, `GetIndexUsage`, `LoadColumnStatsUsage`), plus a separate `cache.TableRowStatsCache` for `TABLE_ROWS` / `DATA_LENGTH` / `INDEX_LENGTH`; and (3) ANALYZE-setup reads in `builder.go` (`StatsMetaCountAndModifyCount`, `GetPhysicalTableStats` for sample-rate sizing) and `analyze.go` (`GetLockedTables` to filter tasks). Most read sites tolerate any staleness and degrade gracefully to NULL / pseudo / "no row"; only sample-rate selection has a hard correctness implication when stats meta diverges from PD-reported row counts.

## Read sites by file

### pkg/executor/builder.go

- builder.go:3160 `executorBuilder.buildAnalyzeSamplingPushdown` reads <table RealtimeCount and ModifyCount> [via `statsHandle.StatsMetaCountAndModifyCount(tid)`]
  Stage: Executor build (lowering plannercore.Analyze to AnalyzeColumnsExec) -- initial baseline before incremental ANALYZE
  Tolerance: low -- used as `baseCount` / `baseModifyCnt` plumbed into the analyze worker so post-analyze delta math is correct
  Fallback: returns error and aborts executor build (`b.err = err; return nil`)
  Notes: failpoints `injectBaseCount` / `injectBaseModifyCount` override the values in tests; this is technically a read site inside the ANALYZE setup, not the write itself.

- builder.go:3267, 3269 `executorBuilder.getAdjustedSampleRate` reads <table RealtimeCount via stats Table> [via `statsHandle.GetPhysicalTableStats(tid, tblInfo)`]
  Stage: Executor build (sample-rate selection for ANALYZE)
  Tolerance: medium -- divergence from PD count triggers the workaround branch at line 3284; otherwise drives `min(1, DefRowsForSampleRate / RealtimeCount)`
  Fallback: if `statsTbl == nil` and no PD answer -> default 0.001; if `RealtimeCount == 0` and no PD answer -> sample-rate 1
  Notes: documented workaround for #29216; reads the entire stats Table object but only inspects `RealtimeCount`.

- builder.go:4778 `buildIndexUsageReporter` reads <UsedStatsInfo map for the session> [via `sc.GetUsedStatsInfo(false)`]
  Stage: Executor build (wraps the per-plan-node IndexUsageReporter that runs at Close)
  Tolerance: any -- `UsedStatsInfo` is the snapshot the planner captured for this statement (may include `Version == PseudoVersion`)
  Fallback: nil reporter is created if `IndexUsageCollector` or `RuntimeStatsColl` is missing -- index usage is silently not recorded
  Notes: `loadStats=true` callers (3801, 4328, 4575, 4736, 5752) trigger `plan.LoadTableStats(ctx)` first to populate the map for plans that bypassed the planner stats-load step (e.g. plan-cache hits, point-gets).

### pkg/executor/internal/exec/indexusage.go

- indexusage.go:120 `IndexUsageReporter.getTableRowCount` reads <table RealtimeCount, stats version> [via `e.statsMap.GetUsedInfo(tableID)` -> `stmtctx.UsedStatsInfo`]
  Stage: Executor.Close -- index-usage reporting after the plan finishes
  Tolerance: any -- uses whatever the planner captured for this statement
  Fallback: returns `(0, false)` if `statsMap` is nil, no entry, or `stats.Version == statistics.PseudoVersion`; `ReportCopIndexUsage` then skips reporting entirely
  Notes: `ReportPointGetIndexUsage` instead substitutes `math.MaxInt32` so a point-get with rows>0 still lands in the smallest non-zero bucket -- different fallback policy from the cop path.

### pkg/executor/show.go

- show.go:797 `ShowExec.fetchShowIndex` reads <stats Table snapshot> [via `h.GetPhysicalTableStats(tb.Meta().ID, tb.Meta())`]
  Stage: SHOW INDEX
  Tolerance: any -- fills the `Cardinality` column from `colStats.NDV`
  Fallback: if `colStats := statsTbl.GetCol(colID)` returns nil, `ndv` stays 0 (column shows `0` cardinality, not NULL)
  Notes: handles both the clustered-PK path (line 813) and each index column (line 882).

### pkg/executor/show_stats.go

- show_stats.go:69, 75, 81 `ShowExec.fetchShowStatsMeta` reads <stats meta row: Version, ModifyCount, RealtimeCount, LastAnalyzeVersion> [via `h.GetNonPseudoPhysicalTableStats(id)`]
  Stage: SHOW STATS_META
  Tolerance: any
  Fallback: pseudo or missing -> skipped (`found=false` and `appendTableForStatsMeta` also early-returns on `nil || Pseudo`); table is simply absent from output

- show_stats.go:169 `ShowExec.fetchShowStatsLocked` reads <lock status set> [via `h.GetLockedTables(tids...)`]
  Stage: SHOW STATS_LOCKED
  Tolerance: any -- read once for the full table list, then iterated
  Fallback: returns error from `GetLockedTables` if storage read fails; on empty map nothing is appended

- show_stats.go:202, 205, 210 `ShowExec.fetchShowStatsHistogram` reads <column/index Histograms: LastUpdateVersion, NDV, NullCount, Correlation, MemoryUsage, StatsLoadedStatus> [via `h.GetPhysicalTableStats(id, tbl)` and `ForEachColumnImmutable` / `ForEachIndexImmutable`]
  Stage: SHOW STATS_HISTOGRAMS
  Tolerance: any
  Fallback: pseudo table -> early return; column/index with `!IsStatsInitialized()` skipped
  Notes: also reads `cardinality.AvgColSize(col, statsTbl.RealtimeCount, false)` (line 226) -- a planner helper that internally reads only the passed-in column object.

- show_stats.go:283, 288, 295 `ShowExec.fetchShowStatsBuckets` reads <histogram bucket data: Count, Repeat, Lower, Upper, NDV> [via `h.GetPhysicalTableStats` then `StableOrderColSlice / StableOrderIdxSlice`]
  Stage: SHOW STATS_BUCKETS
  Tolerance: any
  Fallback: pseudo -> no rows

- show_stats.go:346, 351, 358 `ShowExec.fetchShowStatsTopN` reads <TopN per col/idx> [via `h.GetPhysicalTableStats` -> `col.TopN` / `idx.TopN`]
  Stage: SHOW STATS_TOPN
  Tolerance: any
  Fallback: pseudo -> no rows; nil TopN -> no rows

- show_stats.go:484, 490, 496 `ShowExec.fetchShowStatsHealthy` reads <stats health derived from ModifyCount / RealtimeCount> [via `h.GetNonPseudoPhysicalTableStats(id)` then `statsTbl.GetStatsHealthy()`]
  Stage: SHOW STATS_HEALTHY
  Tolerance: any
  Fallback: pseudo/missing -> skipped; `GetStatsHealthy` returns `(_, false)` -> skipped

- show_stats.go:519 `ShowExec.fetchShowHistogramsInFlight` reads <count of fake stats items pending sync-load> [via `statsStorage.CleanFakeItemsForShowHistInFlights(statsHandle)`]
  Stage: SHOW HISTOGRAMS_IN_FLIGHT
  Tolerance: any
  Fallback: shows 0
  Notes: function name says "Clean" but it returns a count -- it sweeps stale singleflight markers; the read is implicit in the sweep.

- show_stats.go:538 `ShowExec.fetchShowColumnStatsUsage` reads <predicate-column usage map: LastUsedAt, LastAnalyzedAt> [via `h.LoadColumnStatsUsage(loc)`]
  Stage: SHOW COLUMN_STATS_USAGE
  Tolerance: any -- reads `mysql.column_stats_usage` system table via the handle
  Fallback: returns error if storage read fails; columns without an entry are not emitted

- show_stats.go:523 `ShowExec.fetchShowAnalyzeStatus` delegates to `dataForAnalyzeStatusHelper` (see infoschema_reader.go entry below)

### pkg/executor/infoschema_reader.go

- infoschema_reader.go:656, 715, 1325-1326, 1372-1373 `updateStatsCacheIfNeed` / `setDataFromOneTable` / `setDataFromPartitions` read <per-table row-count, data-length, index-length> [via `cache.TableRowStatsCache.UpdateByID` then `EstimateDataLength` / `GetTableRows` / `GetDataAndIndexLength`]
  Stage: INFORMATION_SCHEMA.TABLES and INFORMATION_SCHEMA.PARTITIONS scan
  Tolerance: any -- cached aggregation, refreshed on demand via `UpdateByID`
  Fallback: missing -> 0 row count, 0 lengths (numbers, not NULL); the v2-fast-path at line 855 always emits NULL for these columns
  Notes: this cache lives in `pkg/statistics/handle/cache` but is a separate aggregator from the main stats cache; the v2 short-circuit at 829 skips the read entirely when only schema/table columns are projected.

- infoschema_reader.go:2580 `dataForAnalyzeStatusHelper` reads <statistics.AnalyzeRunning state literal> [via package-level constant comparison]
  Stage: SHOW ANALYZE STATUS / IS.ANALYZE_STATUS
  Tolerance: any -- string comparison against the `state` column read from `mysql.analyze_jobs`
  Fallback: comparison fails -> progress columns left as `any(nil)`
  Notes: not a stats-cache read; just a constant.

- infoschema_reader.go:2647, 2649 `getRemainDurationForAnalyzeStatusHelper` reads <table RealtimeCount> [via `statsHandle.GetPhysicalTableStats(tid, meta)`]
  Stage: IS.ANALYZE_STATUS / SHOW ANALYZE STATUS -- remaining-duration estimation
  Tolerance: any
  Fallback: if `statsTbl == nil` or `RealtimeCount == 0`, falls back to PD's `GetApproximateTableCountFromStorage`; if that also yields 0, `calRemainInfoForAnalyzeStatus` returns `(0, 100.0)` -- i.e. 100% progress shown

- infoschema_reader.go:4022 `memtableRetriever.setDataFromIndexUsage` reads <per-(table,index) usage counters: QueryTotal, KvReqTotal, RowAccessTotal, PercentageAccess, LastUsedAt> [via `dom.StatsHandle().GetIndexUsage(tbl.ID, idx.ID)`]
  Stage: INFORMATION_SCHEMA.TIDB_INDEX_USAGE scan
  Tolerance: any
  Fallback: `GetIndexUsage` returns zero-valued struct -> all counters 0, `LastUsedAt` shown as NULL
  Notes: `setDataFromClusterIndexUsage` (4048) wraps this for the CLUSTER_INDEX_USAGE variant.

### pkg/executor/adapter_slow_log.go

- adapter_slow_log.go:263 `getEncodedPlanAndItemsForSlowLog` (the slow-log builder) reads <UsedStatsInfo map for the statement> [via `stmtCtx.GetUsedStatsInfo(false)`]
  Stage: slow-log / statement summary post-execution
  Tolerance: any -- pure snapshot from session
  Fallback: `nil` map -> slow-log entry has no UsedStats field populated

### pkg/executor/analyze.go

- analyze.go:496 `getLockedTableAndPartitionIDs` reads <lock status for a batch of table/partition ids> [via `statsHandle.GetLockedTables(tidAndPids...)`]
  Stage: Executor build for ANALYZE -- filters out locked targets before dispatching analyze tasks
  Tolerance: any -- fresh read inside the analyze-prep transaction
  Fallback: returns error; analyze aborts
  Notes: included as a read site because lock-status is the stat being queried, even though the surrounding statement is a writer.

### pkg/executor/plan_replayer.go

- plan_replayer.go:489 `PlanReplayerLoadInfo.loadStats` reads <stats handle pointer> to invoke `LoadStatsFromJSON`
  Stage: PLAN REPLAYER LOAD (restore phase)
  Tolerance: must be loaded -- fails with "plan replayer: handle is nil"
  Fallback: returns error if handle nil; unmarshal failures append a warning and continue
  Notes: not a stats-cache read in the usual sense -- it loads JSON into the cache. The plan-replayer DUMP path lives in `adapter.go` (TblStats from stmtCtx) and `domain.DumpPlanReplayerInfo` (out of scope).

### pkg/executor/adapter.go

- adapter.go:2594 `sendPlanReplayerDumpTask` reads <per-table UsedStatsInfo entries> [via `stmtCtx.TableStats`]
  Stage: plan-replayer continuous capture
  Tolerance: any -- uses the same captured snapshot the planner built
  Fallback: nil map -> dump task has no stats
  Notes: `TableStats` is the same surface as `UsedStatsInfo` but exposed as the typed map populated by `GetUsedStatsInfo`.

## Write paths (excluded from deep analysis, listed for completeness)

- pkg/executor/analyze.go -> orchestrates ANALYZE (writes stats meta + histograms via the handle)
- pkg/executor/analyze_col_sampling.go -> column sample-based histogram/TopN/CMSketch construction (write)
- pkg/executor/analyze_global_stats.go -> merges partition stats into global stats (write)
- pkg/executor/analyze_idx.go -> index histogram construction (write)
- pkg/executor/analyze_utils.go -> shared analyze helpers (write helpers)
- pkg/executor/analyze_worker.go -> analyze pipeline worker (write)
- pkg/executor/load_stats.go -> LOAD STATS via `h.LoadStatsFromJSON` (write)
- pkg/executor/lockstats/lock_stats_executor.go -> LOCK STATS via `h.LockTables` / `h.LockPartitions` (write)
- pkg/executor/lockstats/unlock_stats_executor.go -> UNLOCK STATS via `h.RemoveLockedTables` / `h.RemoveLockedPartitions` (write)
- pkg/executor/importer/table_import.go:1063 `FlushTableStats` -> `statshandle.AttachStatsCollector` + `TxnCtx.UpdateDeltaForTable` (writes delta into the collector pipeline; called from `import_into.go:325`)
- pkg/executor/simple.go:2862-2870 `RefreshStats` admin command -> `h.InitStatsLite` / `h.InitStats` (cache rebuild, behaves like a write into the in-memory cache)
- pkg/executor/simple.go:2990 `executeFlushStatsDeltaOnCurrentInstance` -> `h.DumpStatsDeltaToKV` (write)
- pkg/executor/simple.go:3121-3124 `executeDropStats` -> `h.DeleteTableStatsFromKV` then `h.Update` (write, then post-write cache refresh)

## Out of pattern

- builder.go:3160 / 3267 / 3269 are reads embedded inside an ANALYZE-setup path. They are read-side calls but only ever execute as part of a write statement. Whether the contract treats them as reads or as "ANALYZE-internal" is a design call.
- show_stats.go:519 (`CleanFakeItemsForShowHistInFlights`) is named like a writer but the executor uses it as a read (returning a single count). The cleanup side effect happens unconditionally when the user runs `SHOW HISTOGRAMS_IN_FLIGHT`.
- `cache.TableRowStatsCache` (used in infoschema_reader.go) is a parallel cache distinct from the main `statsHandle` stats cache, queried only by `INFORMATION_SCHEMA.TABLES` / `PARTITIONS`. A contract for "reading stats" should explicitly cover this second surface.
- adapter_slow_log.go:263 and adapter.go:2594 read the planner-captured `UsedStatsInfo` snapshot, not the live cache; these are arguably "session-cached stats reads" rather than handle reads. Same shape applies to the `indexUsageReporter` family (builder.go:4778, indexusage.go:120).
- analyze.go:496 reads lock status to make a write decision. Bucketing depends on whether we contract on "what is read" or "for what purpose".
- point_get.go:83 / 190 pass `loadStats=false` to `buildIndexUsageReporter`, so the synchronous `LoadTableStats` preload is skipped for point-get; all other physical operators set `true`. This is intentional but worth noting because it is the only stats-load divergence in the executor path.

## Open questions

- Is `cache.TableRowStatsCache` a separate top-level contract (parallel to `StatsHandle`) or a subordinate detail to mention inside the main contract? The `UpdateByID` call inside `updateStatsCacheIfNeed` does a synchronous batch read from `mysql.stats_meta` / `mysql.stats_histograms`; this happens once per `SELECT ... FROM INFORMATION_SCHEMA.TABLES` invocation.
- `GetPhysicalTableStats` is the read primitive for almost every admin surface in this directory. Is the intended contract "always returns a non-nil `*statistics.Table`, possibly with `Pseudo=true`" (which is how `show.go:797` treats it -- it never nil-checks) versus the partial nil-check at builder.go:3273 and infoschema_reader.go:2652? Worth pinning down.
- `getAdjustedSampleRate` is the only place I found in the executor where stats-meta staleness has a hard correctness consequence (drives sample rate, hence histogram quality). Should the contract call this out as the canonical "RealtimeCount must be reasonably fresh" caller?
- `LoadColumnStatsUsage` (show_stats.go:538) and `GetIndexUsage` (infoschema_reader.go:4022) read auxiliary stats that are independent of the histogram cache. Are these in the same contract or separate ones?
- The slow-log and plan-replayer continuous-capture readers (`adapter.go:2594`, `adapter_slow_log.go:263`) consume `UsedStatsInfo` after execution. Should the contract guarantee that this snapshot remains stable until the executor returns, even if the underlying cache rotates? Today the snapshot is per-`StatementContext` and immutable after planning, but that invariant is not documented anywhere I found in this directory.
