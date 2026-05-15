# Sync Load PR Cross-Reference

Investigation notes produced during Phase 0 of the stats-load contract effort.
For each historical PR in `sync-load-history.txt`, verifies that the fix is
still present in the current tree and captures the contract clause it
established. Source data: `gh pr view`, `gh pr diff`, and direct code reads in
this worktree.

Status legend: PRESENT (intact), REFACTORED (intent preserved, code reshaped),
MISSING (gone), UNCLEAR (could not confirm).

## PR #50956 (2024-02-04, "avoid thundering herd: randomize retry sleep")

- Contract clause: After a failed task in `SubLoadWorker`, the worker sleeps
  `Lease/10 + rand(0..500)µs` instead of a fixed `Lease/10` to spread retry
  storms.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:270-273`
- Status: PRESENT
- Test added: none
- Notes: Sleep bound is hard-coded; contract test should assert the sleep is
  bounded and that two consecutive failures do not produce identical sleep
  durations.

## PR #51911 (2024-03-20, "meta-load case in ColumnIsLoadNeeded")

- Contract clause: `ColumnIsLoadNeeded` takes a `fullLoad` flag; a meta-loaded
  column whose caller only needs meta MUST report `loadNeeded=false`.
- Current code: `pkg/statistics/table.go:826-862`
  (signature `(id int64, fullLoad bool) (col *Column, loadNeeded, hasAnalyzed bool)`)
- Status: PRESENT (signature extended by PR #54531 to also return `hasAnalyzed`)
- Test added: none specific
- Notes: Behavior depends on `col.statsInitialized` for meta-only checks and
  `col.IsFullLoad()` for full-load checks; both branches at lines 854-858.

## PR #52301 (2024-04-03, "singleflight: refactor with the official library")

- Contract clause: Duplicate concurrent sync-load tasks for the same key MUST
  be deduplicated via `singleflight.Group.DoChan`, not the ad-hoc `WorkingColMap`.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:95,125`
  (`globalStatsSyncLoadSingleFlight singleflight.Group`)
- Status: PRESENT (later expanded by PR #52796 to wrap the channel send too)
- Test added: rewritten `TestConcurrentLoadHistWithPanicAndFail` in
  `stats_syncload_test.go` (still present at line 125)
- Notes: Original local `WorkingColMap` removed; `setWorking`/`finishWorking`
  no longer exist.

## PR #52427 (2025-04-15, "build fake column for pseudo estimation")

- Contract clause: When a column is not yet analyzed but `loadNeeded`, the
  worker MUST install an `EmptyColumn` placeholder in the stats cache so
  estimation falls back to pseudo without re-triggering sync load.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:399-403`
  (`statistics.EmptyColumn(...)` then `updateCachedItem`)
- Status: PRESENT (helper extracted into `statistics.EmptyColumn` by PR #57803
  at `pkg/statistics/column.go:250-258`)
- Test added: `TestBuiltinInEstWithoutStats` at
  `pkg/planner/cardinality/selectivity_test.go:2182`
- Notes: Also added `col.ID <= 0` skip in `collect_column_stats_usage.go:145`
  to avoid plan-generated columns; skip survives at line 145.

## PR #52658 (2025-04-22, "upper bound of retry")

- Contract clause: A failing or timing-out task MUST be retried at most
  `RetryCount` times; after that, the task's `ResultCh` MUST receive a result
  with `Error` set.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:53,309,317-320`
  (`isVaildForRetry` increments `task.Retry`)
- Status: PRESENT (budget shrunk over time: 3 → 2 by #57712 → 1 by #60018)
- Test added: `TestRetry` at
  `pkg/statistics/handle/syncload/stats_syncload_test.go:240`
- Notes: Test uses `mockReadStatsForOneFail`; loops `RetryCount*5` which is
  now `5`, so "retries actually exhaust" coverage is thinner with `RetryCount=1`.

## PR #52830 (2025-04-23, "avoid concurrently using the session in syncload")

- Contract clause: Each in-flight sync-load task MUST acquire its own session
  from the pool (and release it on completion); sessions MUST NOT be shared
  across concurrent goroutines.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:331-339`
  (`SPool().WithSession(... WithSessionContext ...)`)
- Status: REFACTORED (callback-based pattern replaced explicit Get/Put;
  invariant preserved)
- Test added: extension to `TestConcurrentLoadHistWithPanicAndFail`
- Notes: Characterization test should assert no panic when many concurrent
  tasks race for sessions.

## PR #52796 (2024-04-24, "global singleflight for sync load")

- Contract clause: Singleflight deduplication MUST happen at request submission
  (`SendLoadRequests`), not just inside the worker, so duplicate items never
  enter the queue.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:125-147` and
  `pkg/parser/model/model.go` `TableItemID.Key()` / `StatsLoadItem.Key()`
- Status: PRESENT
- Test added: changes to `TestConcurrentLoadHistWithPanicAndFail`; new goleak
  ignore.
- Notes: `sc.StatsLoad.ResultCh` is `[]<-chan singleflight.Result`, not a
  single channel. Important for tests that probe per-item results.

## PR #53142 (2024-05-10, "adaptive sync load concurrency")

- Contract clause: When `Performance.StatsLoadConcurrency == 0`, server
  bootstrap MUST size sync-load workers via `GetSyncLoadConcurrencyByCPU()`
  (5/6/8/10 by CPU bucket).
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:55-66`
  (function) and `pkg/session/session.go:4369`
- Status: PRESENT (`DefStatsLoadConcurrencyLimit = 0` at
  `pkg/config/config.go:84-85`)
- Test added: changes to `TestPlanStatsLoadTimeout` (uses `-1` for "no worker")
- Notes: Negative value is the documented "no worker" test sentinel.

## PR #51636 (2024-06-04, "high priority for sync load internal SQLs")

- Contract clause: Internal SQLs issued by sync load (`HistMeta`, `Histogram`,
  `CMSketchAndTopN`) MUST use `kv.PriorityHigh`; worker sessions also tag
  `StmtCtx.Priority = HighPriority`.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:333,444,464,469,474`
  (`HistMetaFromStorageWithHighPriority`,
  `HistogramFromStorageWithPriority(..., kv.PriorityHigh)`,
  `CMSketchAndTopNFromStorageWithHighPriority`)
- Status: PRESENT (priority now per-task via `WithSession` wrapper at line 333;
  the once-on-startup setting in `StartLoadStatsSubWorkers` was removed)
- Test added: none
- Notes: Worth a regression test that `mysql.HighPriority` is set during
  `handleOneItemTask`.

## PR #54531 (2024-08-27, "fix sync load fails after disabling lite init stats")

- Contract clause: When `colInfo` is missing from `ColAndIdxExistenceMap`
  (because lite-init was disabled), sync load MUST fetch `colInfo` from the
  latest infoschema via `TableInfoByID`/`GetColumnByID`.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:359-388`;
  fallback path at lines 385-387 (`wrapper.colInfo = tblInfo.GetColumnByID(item.ID)`)
- Status: PRESENT (further refactored by #54514 to call `GetLatestInfoSchema`)
- Test added: integration check + `selectivity_test.go` changes
- Notes: Also added `(*TableInfo).GetColumnByID` in `pkg/meta/model/table.go`.

## PR #54514 (2024-09-30, "avoid using infoschema when initialising stats")

- Contract clause: `InitStats`/`InitStatsLite` MUST NOT depend on infoschema;
  existence-map entries are written from `mysql.stats_histograms`/`stats_meta`
  directly.
- Current code: `pkg/statistics/handle/bootstrap.go:160,168,264,286`
  (`ColAndIdxExistenceMap.InsertCol/InsertIndex`); both lite and full paths
  populate the map from rows.
- Status: REFACTORED (rewritten by #57803; intent preserved)
- Test added: re-tuned existing init-stats tests
- Notes: `InitStatsLite` at `bootstrap.go:833`, `InitStats` at `:894`; both
  take optional `tableIDs ...int64` since this PR.

## PR #56614 (2024-10-14, "avoid unnecessary try when to sync load")

- Contract clause: If `readStatsForOneItem` returns `errGetHistMeta` (hist
  meta row missing in storage), the worker MUST treat it as a successful
  no-op rather than an error to retry.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:431` (var),
  `:409-411` (caller swallows), `:455` (return inside `readStatsForOneItem`)
- Status: PRESENT
- Test added: SQL-level regression in
  `tests/integrationtest/t/planner/core/issuetest/planner_issue.test`
  (TestIssue56472 block)
- Notes: Log downgraded from `Error` to `Warn`
  (`StatsSampleLogger().Warn` at line 449).

## PR #57144 (2024-11-21, "avoid sync load column with skip type")

- Contract clause: Before loading, the worker MUST consult
  `TiDBAnalyzeSkipColumnTypes` and skip columns whose type/charset is in that set.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:345-351,389-394`
  (reads `vardef.TiDBAnalyzeSkipColumnTypes`, then `types.TypeToStr` lookup)
- Status: PRESENT
- Test added: `TestSyncLoadSkipAnalyzSkipColumnItems` in `stats_syncload_test.go`
- Notes: Installed the `handleOneItemTaskPanic` failpoint at line 405 used by
  the test; tests can rely on it to inject panics for chaos coverage.

## PR #57712 (2024-11-27, "rightly deal with timeout when sending sync load; retry=2")

- Contract clause: After pushing a task into `neededItemsCh`, the goroutine
  running inside `singleflight.DoChan` MUST also respect the outer timer; if
  `<-timer.C` fires while waiting on `task.ResultCh`, it returns
  "sync load took too long to return".
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:133-145`
  (nested `select` on `timer.C` and `task.ResultCh`)
- Status: PRESENT (RetryCount=2 from this PR later reduced to 1 by #60018)
- Test added: `TestSendLoadRequestsWaitTooLong` at
  `stats_syncload_test.go:331`
- Notes: Without this nested `select`, a stalled worker would let the caller
  hang forever after the send succeeded.

## PR #57723 (2024-11-27, "fix some problems related to stats async load")

- Contract clause: `Version0` (unanalyzed) columns MUST NOT update
  `LastAnalyzeVersion`; async-load must early-return for not-analyzed columns
  and write a fake column for pseudo estimation.
- Current code: `pkg/statistics/handle/storage/read.go`
  `loadNeededColumnHistograms` (analyzed/`Version0` guards) and
  `pkg/statistics/column.go:250-258` `EmptyColumn`
- Status: PRESENT
- Test added: integration tests in `pkg/statistics/integration_test.go`
  (`TestLastAnalyzeVersion`)
- Notes: Async sibling of #52427's sync fix; same `EmptyColumn` helper, so a
  single behavioral test can cover both surfaces.

## PR #57803 (2024-12-03, "non-lite InitStats and stats sync load of no-stats column")

- Contract clause: After `InitStats`/`InitStatsLite`, the post-state MUST be
  uniform: index stats fully loaded; column existence recorded in
  `ColAndIdxExistenceMap`; column histogram NOT loaded; `Pseudo` should be
  false even for a fresh empty table.
- Current code: `pkg/statistics/handle/bootstrap.go:827-833`
  (`InitStatsLite` doc), `:887-901` (`InitStats` doc), `:244-262`
  (4Chunk loader with `Version0` vs analyzed branches)
- Status: REFACTORED but core invariants intact; SQL generator helpers
  (`initStatsHistogramsSQLGen`, `initStatsTopNSQLGen`, `initStatsBucketsSQLGen`)
  introduced by the PR have been replaced. Loader still uses
  `IsColumnAnalyzedOrSynthesized` to gate `InsertCol` (lines 168, 286).
- Test added: `TestInitStats` and `TestInitStatsVer2` updates in
  `statstest/stats_test.go`
- Notes: Most invasive PR in the set. Any characterization test should assert
  that all three init modes (lite, non-lite, concurrent) leave the existence
  map populated identically.

## PR #59031 (2025-01-21, "fix DROP STATS after #58596")

- Contract clause: `DROP STATS` MUST be a "soft" delete
  (`DeleteTableStatsFromKV(ids, soft=true)`); a dropped table reads back with
  `Pseudo=false`, `StatsVer=Version0`, but per-column
  `IsStatsInitialized()==false`.
- Current code: `pkg/executor/simple.go:3121`
  (`h.DeleteTableStatsFromKV(statsIDs, true)`);
  `pkg/statistics/handle/storage/gc.go:65-69,133-135` define the soft
  parameter; auto-GC path at `gc.go:289` still calls with `soft=false`
- Status: PRESENT
- Test added: rewritten `TestDropStats` and `TestDropGlobalStats` in
  `pkg/executor/test/simpletest/simple_test.go`
- Notes: Tightly coupled to issue #58596 (drop of live column-size updates).
  Test must cover post-DROP read path: no pseudo flag, but no buckets either.

## PR #60018 (2025-03-11, "reduce max retry count to 1")

- Contract clause: `RetryCount = 1`; commentary clarifies that retrying is not
  useful because parallel requests will retry on their own.
- Current code: `pkg/statistics/handle/syncload/stats_syncload.go:48-53`
- Status: PRESENT
- Test added: none (config-only change)
- Notes: With `RetryCount=1`, only one retry; `task.Retry++` then exceeds and
  the caller writes the error to `ResultCh`. `TestRetry` still passes because
  it manually resets `task.Retry`. TODO comment at lines 48-52 hints the next
  step is `0` / removal.

## Cross-cutting observations

- Retry policy churn: `RetryCount` went `?` → `3` (#52658) → `2` (#57712) →
  `1` (#60018). Tests should treat `RetryCount` as the contract, not the
  constant.
- Timeout/cancellation patched three times: (1) #52301 library singleflight,
  (2) #52796 singleflight before the channel send, (3) #57712 inner `select`
  on the result channel. Only `TestSendLoadRequestsWaitTooLong` covers the
  full "happy path → stuck worker → client times out" trio. Recommended
  characterization tests: per-stage timeout (queue-full, send-OK-worker-stalled,
  result-pending-after-send).
- Singleflight key shape: `TableItemID.Key() = "%d#%d#%t"` and
  `StatsLoadItem.Key()` appends `FullLoad`. Meta-load and full-load are
  deduplicated separately by design, worth a test.
- `EmptyColumn`/pseudo fallback invoked from two places: sync at
  `stats_syncload.go:399-403`, async at `storage/read.go`
  `loadNeededColumnHistograms`. PR #52427 introduced sync-side, #57723
  mirrored async-side. One behavioral test exercising both is appropriate.
- Conflicting / superseded fixes:
  - #51636 set `StmtCtx.Priority = HighPriority` in `StartLoadStatsSubWorkers`;
    #52830 deleted that line and moved priority assignment per-task. Current
    code's per-task assignment (line 333) is the surviving contract.
  - #52301's `WorkingColMap` / `Singleflight` field on `StatsLoad` was
    discarded by #52796 in favor of a global package-level singleflight.
    Reading #52301 in isolation finds non-existent symbols.
  - #54514 ("avoid using infoschema") was rewritten by #57803; the SQL-gen
    helpers introduced by #57803 are no longer in the file.
- Fragile invariants without direct tests:
  - Worker sleep bounds (#50956): no assertion on sleep duration distribution.
  - Per-task `mysql.HighPriority` set in `handleOneItemTask` (#51636): relies
    on `defer` to reset; nothing asserts the reset.
  - `errGetHistMeta` swallowing (#56614): only integration scenario, no unit
    assertion that other read errors are NOT swallowed.

## Test surface (current)

In `pkg/statistics/handle/syncload/stats_syncload_test.go`:

- `TestConcurrentLoadHistWithPanicAndFail` (line 125)
- `TestRetry` (line 240)
- `TestSendLoadRequestsWaitTooLong` (line 331)
- `TestSyncLoadSkipUnAnalyzedItems`
- `TestSyncLoadSkipAnalyzSkipColumnItems`
- `TestConcurrentLoadHist`

These six are the entire local unit-test coverage; everything else lives in
integration / casetest packages.

## Net status

17 PRs verified: 14 PRESENT, 3 REFACTORED, 0 MISSING. None of the original
contract clauses have been outright removed.
