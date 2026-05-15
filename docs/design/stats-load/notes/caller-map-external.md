# External-package stats call sites (ddl, domain, session, server, util, ttl, dxf, table, store)

## Summary

Outside the planner/executor surface, almost all statistics reads are concentrated in `pkg/domain/` (the `Handle` lifecycle and the background workers) and the `pkg/server/` HTTP dump endpoints. The `pkg/ddl/` package only stores a handle pointer (it is registered but no read sites exist in non-test code), `pkg/session/` only attaches per-session delta collectors, `pkg/sessionctx/` defines structured copies of stats-meta fields used by stmtctx, and `pkg/util/*`, `pkg/ttl/`, `pkg/dxf/`, `pkg/table/`, `pkg/store/` either do not read statistics or read auxiliary structures (TTL job counters, mockstore analyze builders). The single concrete table-stats read outside planner/executor is `Domain.GetPhysicalTableStats` for plan-replayer dumps.

## Read sites by package

### pkg/ddl

`pkg/ddl/` non-test code does not read statistics. The handle pointer is stored for downstream consumers (notifier, executors) but never dereferenced in this package.

- pkg/ddl/ddl.go:360 `ddlCtx` declares `statsHandle *handle.Handle` field
  Stage: setup
  Tolerance: n/a
  Fallback: nil -- never dereferenced here
- pkg/ddl/ddl.go:677-679 `(*ddl).RegisterStatsHandle` stores the handle into `ddlCtx` and `executor`
  Stage: bootstrap (called from `Domain.UpdateTableStatsLoop`)
  Tolerance: n/a (not a read)
  Fallback: n/a
- pkg/ddl/executor.go:187 `executor.statsHandle *handle.Handle` field declaration only
  Notes: searched all `pkg/ddl/**` non-test files; no `statsHandle.X(...)` invocation found, so any stats invalidation/relocation on DDL events flows through the notifier subscription handlers registered in `pkg/statistics/handle/...`, not through this field.

### pkg/domain

- pkg/domain/domain.go:151 `Domain.statsHandle atomic.Pointer[handle.Handle]` field
  Stage: lifecycle
- pkg/domain/domain.go:526 `(*Domain).Close` closes `do.statsHandle.Load()` if set
  Stage: shutdown
- pkg/domain/domain.go:1870 `(*Domain).HistoricalStatsWorker` calls `do.StatsHandle()` then `DumpHistoricalStats`
  Stage: background worker
  Tolerance: snapshot at DDL job time
  Fallback: log warn, skip dump
- pkg/domain/domain.go:1880 `StatsHandle()` accessor (returns nil if not yet created)
  Stage: any
- pkg/domain/domain.go:1885-1900 `CreateStatsHandle` (test-only) constructs and stores a handle
  Notes: test-only entrypoint
- pkg/domain/domain.go:1930-1944 `(*Domain).UpdateTableStatsLoop` constructs the production `Handle`, calls `StartWorker`, stores it, registers with DDL
  Stage: bootstrap (BootstrapSession)
- pkg/domain/domain.go:1970 `waitStartTask` reads `do.StatsHandle().InitStatsDone` channel
  Stage: bootstrap gate
  Tolerance: blocks workers (gc/auto-analyze/cleanup) until InitStats finishes
  Fallback: if `do.exit` first, workers never start
- pkg/domain/domain.go:2018-2021 `StartLoadStatsSubWorkers` calls `statsHandle.SubLoadWorker`
  Stage: bootstrap; sync-load consumer goroutines
- pkg/domain/domain.go:2038-2068 `(*Domain).initStats` -- the canonical init entry: branches on `Performance.SkipInitStats` (closes `InitStatsDone` immediately) and `Performance.LiteInitStats` (chooses `InitStatsLite(ctx)` vs `InitStats(ctx, InfoSchema())`); logs both success and failure but does not propagate errors
  Stage: bootstrap
  Tolerance: cold-start; lazy fallback on lite mode
  Fallback: log error; cache remains whatever the partial load produced; downstream readers see pseudo or partial entries
- pkg/domain/domain.go:2087-2093 `(*Domain).loadStatsWorker` runs `initStats(ctx)` then `statsHandle.Update(ctx, InfoSchema())` on every `statsLease` tick
  Stage: bootstrap + steady-state refresh
  Tolerance: up to one lease behind
  Fallback: log warn; keep previous cache
- pkg/domain/domain.go:2115 `(*Domain).asyncLoadHistogram` waits on `InitStatsDone`, then ticks `statsHandle.LoadNeededHistograms(InfoSchema())`
  Stage: async lazy load
  Tolerance: drains the sync-load wait queue
  Fallback: log warn
- pkg/domain/domain.go:2137-2146 `(*Domain).indexUsageWorker` calls `handle.GCIndexUsage()`
  Stage: background GC
- pkg/domain/domain.go:2179-2185 `deltaUpdateTickerWorkerExitPreprocessing` calls `statsHandle.FlushStats()`
  Stage: shutdown
- pkg/domain/domain.go:2205-2233 `(*Domain).gcStatsWorker` calls `statsHandle.GCStats`, `StatsCache.TriggerEvict`, `UpdateStatsHealthyMetrics`, `CheckAutoAnalyzeWindows`
  Stage: background GC (owner-only for GCStats)
  Tolerance: hourly-ish
  Fallback: log warn
- pkg/domain/domain.go:2244-2256 `(*Domain).dumpColStatsUsageWorker` calls `statsHandle.DumpColStatsUsageToKV()`
  Stage: background write (not a stats read)
- pkg/domain/domain.go:2276-2283 `(*Domain).deltaUpdateTickerWorker` calls `statsHandle.DumpStatsDeltaToKV(false)`
  Stage: background write
- pkg/domain/domain.go:2293-2325 `(*Domain).autoAnalyzeWorker` checks `RunAutoAnalyze` + owner, calls `statsHandle.HandleAutoAnalyze()` / `ClosePriorityQueue()`
  Stage: background; reads priority-queue + stats internally
- pkg/domain/domain.go:2363-2393 `(*Domain).analyzeJobsCleanupWorker` calls `DeleteAnalyzeJobs`, `CleanupCorruptedAnalyzeJobsOnCurrentInstance`, `CleanupCorruptedAnalyzeJobsOnDeadInstances`
  Stage: background GC
- pkg/domain/domain_sysvars.go:55-59 `setStatsCacheCapacity` calls `do.StatsHandle().SetStatsCacheCapacity(c)` on sysvar change
  Stage: sysvar handler
  Fallback: nil-check on first-boot when handle missing
- pkg/domain/historical_stats.go:57-84 `(*HistoricalStatsWorker).DumpHistoricalStats` calls `statsHandle.CheckHistoricalStatsEnable()` and `RecordHistoricalStatsToStorage`
  Stage: post-DDL dump
  Tolerance: best-effort
  Fallback: returns error (caller logs warn)
- pkg/domain/plan_replayer_dump.go:547-575 `dumpStatsMemStatus` calls `statsHandle.GetPhysicalTableStats(tbl.Meta().ID, tbl.Meta())` and then iterates with `ForEachIndexImmutable` / `ForEachColumnImmutable`, reading each `Index.StatusToString()` / `Column.StatusToString()`
  Stage: plan-replayer dump
  Tolerance: snapshot of current in-memory cache
  Fallback: skip table if `tblStats == nil`
- pkg/domain/plan_replayer_dump.go:850-861 `getStatsForTable` calls `h.DumpHistoricalStatsBySnapshot` (when `historyStatsTS > 0`) or `h.DumpStatsToJSON`
  Stage: plan-replayer dump
  Tolerance: snapshot vs latest cache

### pkg/session and pkg/sessionctx

- pkg/session/session.go:4042-4045 `createSessionFunc`-style path attaches `statsCollector` and `idxUsageCollector` from `do.StatsHandle()` when `do.StatsUpdating()` is true
  Stage: session creation
  Tolerance: skipped if handle nil or worker not running
  Fallback: nil collector, no delta capture
- pkg/session/session.go:4766-4772 `attachStatsCollector(s, dom)` (pool reuse path) attaches collectors from `dom.StatsHandle()` if nil and updating
  Stage: session reuse
- pkg/session/session.go:455-459, 973-976, 3401-3405, 3975-3976, 4780-4786, 5631-5635 use the previously-attached `s.statsCollector` / `s.idxUsageCollector` to push deltas/flush/delete
  Stage: per-statement
  Tolerance: writes only; not reads of `*statistics.Table`
- pkg/sessionctx/stmtctx/stmtctx.go:1460-1528 `UsedStatsInfoForTable` holds `Version, RealtimeCount, ModifyCount, ColumnStatsLoadStatus, IndexStatsLoadStatus` and renders them via `FormatForExplain` / `WriteToSlowLog`
  Stage: explain / slow-log emission
  Tolerance: snapshot captured by planner at plan time
  Fallback: `Version==0` => prints `stats:pseudo`
  Notes: this struct is *populated* by the planner (out of scope here) and *read* here when rendering explain/slow-log output; comments explicitly reference `statistics.PseudoVersion == 0`.

The bootstrap files (`pkg/session/bootstrap.go`, `pkg/session/upgrade_def.go`) only contain CREATE-TABLE statements for the `mysql.stats_*` system tables and migration DDL; no stats read.

### pkg/server

- pkg/server/server.go:483-484 `(*Server).run`: if `Performance.ForceInitStats`, blocks on `<-dom.StatsHandle().InitStatsDone` before opening TiDB listener
  Stage: server-start gate
  Tolerance: serializes listener-open against InitStats
  Fallback: when flag off, server opens listener immediately and accepts queries that may see pseudo stats
- pkg/server/http_handler.go:62-66 builds a `statsHandle` reference for `NewPlanReplayerHandler` (nil if domain or handle nil)
  Stage: handler construction
- pkg/server/http_status.go:711-721 `newStatsHandler` constructs `optimizor.NewStatsHandler(do)`
- pkg/server/handler/optimizor/statistics_handler.go:53-81 `StatsHandler.ServeHTTP` (`/stats/dump/{db}/{table}`) calls `sh.do.StatsHandle().DumpStatsToJSON`
  Stage: HTTP request
  Tolerance: current in-memory cache
  Fallback: WriteError
- pkg/server/handler/optimizor/statistics_handler.go:93-137 `StatsHistoryHandler.ServeHTTP` (`/stats/dump/.../{snapshot}`) calls `CheckHistoricalStatsEnable` then `DumpHistoricalStatsBySnapshot`
  Stage: HTTP request
  Tolerance: bounded by `tidb_enable_historical_stats`
  Fallback: 404-style error if disabled
- pkg/server/handler/optimizor/statistics_handler.go:159-168 `StatsPriorityQueueHandler.ServeHTTP` calls `h.GetPriorityQueueSnapshot()`
  Stage: HTTP request
- pkg/server/handler/optimizor/plan_replayer.go:51-81, 216-247 `PlanReplayerHandler` holds a `statsHandle`; `handlePlanReplayerCaptureFile` calls `handler.statsHandle.DumpHistoricalStatsBySnapshot` per table in the capture
  Stage: HTTP download
  Tolerance: historical TS read

### pkg/util/*

`pkg/util/expensivequery/`, `pkg/util/metricsutil/`, `pkg/util/memory/`, `pkg/util/sem/` do not read statistics. Specifically:

- pkg/util/metricsutil/common.go:37-38, 142-143 only imports `statistics/handle/cache/metrics` and `statistics/handle/metrics` packages to call their `InitMetricsVars()` registrars
  Stage: metrics init
  Tolerance: n/a
- pkg/util/memory/tracker.go:899-900 defines `LabelForStatsCache = -15` (memory-tracker label only); no stats read
- pkg/util/sem/sem.go:166 lists `vardef.TiDBStatsCacheMemQuota` in SEM-restricted variables; not a read
- pkg/util/tableutil/tableutil.go:24-46 declares `TempTable` interface with `GetStats() any` returning a `*statistics.Table` opaquely (consumers in planner/executor)
  Stage: interface only
  Notes: avoids cycle import; consumers are out of scope

No other `pkg/util/**` non-test file references `statistics.Handle` / `statistics.Table` / `StatsCache`.

### pkg/ttl

- pkg/ttl/ttlworker/session.go:28, 58-59 `withSession` calls `statshandle.AttachStatsCollector(sctx.GetSQLExecutor())` and `DetachStatsCollector` around TTL-deletion sessions
  Stage: TTL worker session lifecycle
  Tolerance: write path (delta collection), not a stats read
  Fallback: skipped when `intest.InTest` and mock session is used

The remaining `pkg/ttl/ttlworker/*.go` references to `t.statistics.IncErrorRows` / `IncSuccessRows` / `TotalRows.Load()` etc. (`del.go:154,193,199,275,294,306`; `scan.go:228-297`; `task_manager.go:764-792`) are TTL-internal job-progress counters, unrelated to the optimizer statistics subsystem.

### pkg/dxf

- pkg/dxf/importinto/scheduler.go:48, 709-716, 735-737 `(*importScheduler).finishJob` builds a `statsstorage.DeltaUpdate{Delta: variable.TableDelta{...}, TableID: ...}` and calls `statsstorage.UpdateStatsMeta(ctx, se, txn.StartTS(), tableStatsDelta)` to flush the delta
  Stage: post-import finalization
  Tolerance: write path; provides modify-count to auto-analyze trigger
  Fallback: log warn if flush fails (auto-analyze may not fire promptly)
  Notes: no read of an existing `*statistics.Table`; the count comes from `taskMeta.Summary.ImportedRows`

### pkg/table

- pkg/table/tables/tables.go:1911, 1924, 1946-1948 `TemporaryTable.stats *statistics.Table` is constructed as `statistics.PseudoTable(tblInfo, false, false)` and exposed via `GetStats() any`
  Stage: temp-table construction (one-shot)
  Tolerance: pseudo only (temp tables never have real stats)
  Fallback: n/a (always pseudo)
  Notes: this is the only `*statistics.Table` materialization outside the handle, and it returns pseudo unconditionally.

### pkg/store

No stats reads. The matches in `pkg/store/mockstore/mockcopr/analyze.go` and `pkg/store/mockstore/unistore/cophandler/analyze.go` are construction of `statistics.NewSortedBuilder`, `NewCMSketch`, `NewFMSketch`, `HistogramToProto`, `CMSketchToProto`, `SampleCollectorToProto`, and TopN-meta builders inside the analyze coprocessor mock -- i.e. they synthesize stats responses (write/produce side), not read an existing stats cache.

## Init / bootstrap call sites (special category -- server-start vs lazy)

1. pkg/domain/domain.go:1930 `(*Domain).UpdateTableStatsLoop` -- always called once per server start from `BootstrapSession`; creates the handle, registers it with DDL, then conditionally launches background workers when `do.statsLease > 0`.
2. pkg/domain/domain.go:2038 `(*Domain).initStats` -- the only place that decides between full and lite init.
   - If `config.GetGlobalConfig().Performance.SkipInitStats` is true: close `InitStatsDone` immediately, log "Skipping initial stats due to skip-grant-table being set", return. No cache populated.
   - Else if `config.GetGlobalConfig().Performance.LiteInitStats` is true: call `statsHandle.InitStatsLite(ctx)` (no histograms, no TopN -- only stats-meta and column/index metadata).
   - Else: call `statsHandle.InitStats(ctx, do.InfoSchema())` (full load).
   - On error, log and continue; `InitStatsDone` is closed in the deferred recover() path.
3. pkg/domain/domain.go:2087 `(*Domain).loadStatsWorker` is the goroutine that runs `initStats` then enters the lease ticker calling `statsHandle.Update`. Launched only if `do.statsLease >= 0`.
4. pkg/server/server.go:483 if `Performance.ForceInitStats`, the server `run()` blocks on `<-dom.StatsHandle().InitStatsDone` before listening -- this is the only place outside the domain itself that gates startup on stats init.
5. pkg/domain/domain.go:1970, 2115 `waitStartTask` and `asyncLoadHistogram` also gate on `InitStatsDone` so the gc/auto-analyze/analyze-cleanup workers do not run before init completes.
6. pkg/domain/domain.go:1885-1900 `(*Domain).CreateStatsHandle` is a test-only entry that bypasses the bootstrap and constructs a handle without launching workers.

## Out of pattern

- `pkg/ddl/` non-test code stores `statsHandle` (in `ddlCtx` and `executor`) but never dereferences it. All visible DDL/stats interaction routes through `pkg/ddl/notifier/` (which defines `StatsMetaHandlerID = 1`, registered by `pkg/statistics/handle/...`) rather than direct handle calls. Treat the `ddlCtx.statsHandle` field as effectively dead code from this package's perspective.
- `pkg/domain/domain.go:2231` `do.StatsHandle().StatsCache.TriggerEvict()` is the only place outside the handle that directly touches the `StatsCache` field; every other access goes through `Handle` methods. This is invoked from `gcStatsWorker` after `memory.ForceReadMemStats()`, tying eviction to memory pressure.
- `pkg/table/tables/tables.go:1924` constructs a `*statistics.Table` directly via `statistics.PseudoTable(...)` and stores it in `TemporaryTable.stats`. This is the only direct `*statistics.Table` materialization outside `pkg/statistics/**` and `pkg/planner/**`.
- `pkg/sessionctx/stmtctx/stmtctx.go:1460-1528` (`UsedStatsInfoForTable`) is a structured copy of stats-meta fields populated at planning time and read back at explain/slow-log emission. The `Version == 0` check (`PseudoVersion`) is the only pseudo-detection logic outside `pkg/statistics/`.
- `pkg/server/server.go:483` `Performance.ForceInitStats` is the only flag that makes connection acceptance wait for stats. Without it, queries can land on pseudo or partially-loaded stats.

## Open questions

1. Does the `ddlCtx.statsHandle` / `executor.statsHandle` field have any in-tree reader (perhaps via the cross-keyspace or BR code paths not searched), or is it truly dead in mainline? If dead, why is `RegisterStatsHandle` still on the `DDL` interface?
2. `Domain.gcStatsWorker` at `domain.go:2231` calls `do.StatsHandle().StatsCache.TriggerEvict()` directly on the cache field. Should this go through a `Handle` method to preserve the encapsulation that every other call site respects?
3. `Domain.initStats` swallows the InitStats error and only logs it. What is the resulting cache state when `InitStats` fails partway (e.g., for a subset of tables) -- does the planner see pseudo for the unloaded tables or stale loaded state, and does `Handle.Update` later self-repair without re-reading missing histograms?
4. `pkg/server/server.go:484` blocks only when `ForceInitStats` is set. Are there documented user expectations that without this flag, early connections will see pseudo stats and incur sync-load on first reference?
5. `pkg/dxf/importinto/scheduler.go:735` flushes a `DeltaUpdate` whose `Count` and `Delta` are both set to `ImportedRows`. Is that the intended semantics for the auto-analyze trigger (i.e., treating every imported row as both row-count delta and modify-count), or could the read-side trigger logic misinterpret the modify-count?
