// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package partition

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/session"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/stretchr/testify/require"
)

func TestMultiSchemaVerPartitionBy(t *testing.T) {
	distCtx := testkit.NewDistExecutionContextWithLease(t, 2, 15*time.Second)
	store := distCtx.Store
	dom1 := distCtx.GetDomain(0)
	dom2 := distCtx.GetDomain(1)
	defer func() {
		dom1.Close()
		dom2.Close()
		store.Close()
	}()

	ddlJobsSQL := `admin show ddl jobs where db_name = 'test' and table_name = 't' and job_type = 'alter table partition by'`

	se1, err := session.CreateSessionWithDomain(store, dom1)
	require.NoError(t, err)
	se2, err := session.CreateSessionWithDomain(store, dom2)
	require.NoError(t, err)

	// Session on non DDL owner domain (~ TiDB Server)
	tk1 := testkit.NewTestKitWithSession(t, store, se1)
	// Session on DDL owner domain (~ TiDB Server), used for concurrent DDL
	tk2 := testkit.NewTestKitWithSession(t, store, se2)
	// Session on DDL owner domain (~ TiDB Server), used for queries
	tk3 := testkit.NewTestKitWithSession(t, store, se2)
	tk1.MustExec(`use test`)
	tk2.MustExec(`use test`)
	tk3.MustExec(`use test`)
	// The DDL Owner will be the first created domain, so use tk1.
	tk1.MustExec(`create table t (a int primary key, b varchar(255))`)
	dom1.Reload()
	dom2.Reload()
	verStart := dom1.InfoSchema().SchemaMetaVersion()
	alterChan := make(chan struct{})

	// Is it possible to only do this on a single DOM?
	hookFunc := func(job *model.Job) {
		alterChan <- struct{}{}
		<-alterChan
	}
	failpoint.EnableCall("github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", hookFunc)
	defer failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/onJobRunBefore")
	go func() {
		tk1.MustExec(`alter table t partition by hash(a) partitions 3`)
		alterChan <- struct{}{}
	}()
	// Wait for the first state change to begin
	<-alterChan
	alterChan <- struct{}{}
	// Doing the first State change
	stateChange := int64(1)
	<-alterChan
	// Waiting before running the second State change
	verCurr := dom1.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	require.Equal(t, verStart, dom2.InfoSchema().SchemaMetaVersion())
	tk2.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] CLUSTERED */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))
	dom2.Reload()
	require.Equal(t, verCurr, dom2.InfoSchema().SchemaMetaVersion())
	tk2.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] CLUSTERED */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY NONE ()\n" +
		"(PARTITION `pFullTable` COMMENT 'Intermediate partition during ALTER TABLE ... PARTITION BY ...')"))
	tk2.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"delete only", "running"}})
	alterChan <- struct{}{}
	// doing second State change
	stateChange++
	<-alterChan
	// Waiting before running the third State change
	verCurr = dom1.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	dom2.Reload()
	require.Equal(t, verCurr, dom1.InfoSchema().SchemaMetaVersion())
	tk2.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] CLUSTERED */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY NONE ()\n" +
		"(PARTITION `pFullTable` COMMENT 'Intermediate partition during ALTER TABLE ... PARTITION BY ...')"))
	tk2.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"write only", "running"}})
	alterChan <- struct{}{}
	<-alterChan
	stateChange++
	verCurr = dom1.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	dom2.Reload()
	require.Equal(t, verCurr, dom2.InfoSchema().SchemaMetaVersion())
	tk2.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] CLUSTERED */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY NONE ()\n" +
		"(PARTITION `pFullTable` COMMENT 'Intermediate partition during ALTER TABLE ... PARTITION BY ...')"))
	tk2.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"write reorganization", "running"}})
	alterChan <- struct{}{}
	<-alterChan
	stateChange++
	verCurr = dom1.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	dom2.Reload()
	require.Equal(t, verCurr, dom2.InfoSchema().SchemaMetaVersion())
	tk2.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] CLUSTERED */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY HASH (`a`) PARTITIONS 3"))
	tk2.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"delete reorganization", "running"}})
	alterChan <- struct{}{}
	<-alterChan
	// Alter done!
	stateChange++
	verCurr = dom1.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	dom2.Reload()
	require.Equal(t, verCurr, dom2.InfoSchema().SchemaMetaVersion())
	tk2.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] CLUSTERED */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY HASH (`a`) PARTITIONS 3"))
	tk2.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"none", "synced"}})
}

func TestMultiSchemaVerAddPartition(t *testing.T) {
	distCtx := testkit.NewDistExecutionContextWithLease(t, 2, 15*time.Second)
	store := distCtx.Store
	domOwner := distCtx.GetDomain(0)
	domNonOwner := distCtx.GetDomain(1)
	defer func() {
		domOwner.Close()
		domNonOwner.Close()
		store.Close()
	}()

	isOwnerCorrect := domOwner.DDL().OwnerManager().IsOwner()
	if !isOwnerCorrect {
		domOwner, domNonOwner = domNonOwner, domOwner
	}

	seOwner, err := session.CreateSessionWithDomain(store, domOwner)
	require.NoError(t, err)
	seNonOwner, err := session.CreateSessionWithDomain(store, domNonOwner)
	require.NoError(t, err)

	tkDDLOwner := testkit.NewTestKitWithSession(t, store, seOwner)
	tkDDLOwner.MustExec(`use test`)
	tkDDLOwner.MustExec(`set @@global.tidb_enable_global_index = 1`)
	tkDDLOwner.MustExec(`set @@session.tidb_enable_global_index = 1`)
	tkA := testkit.NewTestKitWithSession(t, store, seOwner)
	tkA.MustExec(`use test`)
	tkB := testkit.NewTestKitWithSession(t, store, seNonOwner)
	tkB.MustExec(`use test`)
	tkDDLOwner.MustExec(`create table t (a int primary key nonclustered global, b varchar(255) charset utf8mb4 collate utf8mb4_0900_ai_ci) partition by range columns (b) (partition p0 values less than ("m"))`)
	domOwner.Reload()
	domNonOwner.Reload()
	verStart := domNonOwner.InfoSchema().SchemaMetaVersion()
	alterChan := make(chan struct{})
	hookFunc := func(job *model.Job) {
		alterChan <- struct{}{}
		<-alterChan
	}
	failpoint.EnableCall("github.com/pingcap/tidb/pkg/ddl/onJobRunBefore", hookFunc)
	defer failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/onJobRunBefore")
	go func() {
		tkDDLOwner.MustExec(`alter table t add partition (partition p1 values less than ("p"))`)
		alterChan <- struct{}{}
	}()
	// Wait for the first state change to begin
	<-alterChan
	alterChan <- struct{}{}
	// Doing the first State change
	stateChange := int64(1)
	<-alterChan
	// Waiting before running the second State change
	verCurr := domOwner.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	require.Equal(t, verStart, domNonOwner.InfoSchema().SchemaMetaVersion())
	tkA.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) COLLATE utf8mb4_0900_ai_ci DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] NONCLUSTERED */ /*T![global_index] GLOBAL */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY RANGE COLUMNS(`b`)\n" +
		"(PARTITION `p0` VALUES LESS THAN ('m'))"))
	tkB.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) COLLATE utf8mb4_0900_ai_ci DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] NONCLUSTERED */ /*T![global_index] GLOBAL */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY RANGE COLUMNS(`b`)\n" +
		"(PARTITION `p0` VALUES LESS THAN ('m'))"))
	domNonOwner.Reload()
	require.Equal(t, verCurr, domNonOwner.InfoSchema().SchemaMetaVersion())
	tkB.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) COLLATE utf8mb4_0900_ai_ci DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] NONCLUSTERED */ /*T![global_index] GLOBAL */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY RANGE COLUMNS(`b`)\n" +
		"(PARTITION `p0` VALUES LESS THAN ('m'))"))
	ddlJobsSQL := `admin show ddl jobs where db_name = 'test' and table_name = 't' and job_type = 'add partition'`
	tkB.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"replica only", "running"}})
	alterChan <- struct{}{}
	stateChange++
	// Alter is completed
	<-alterChan
	tkB.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"public", "synced"}})
	verCurr = domOwner.InfoSchema().SchemaMetaVersion()
	require.Equal(t, stateChange, verCurr-verStart)
	tkDDLOwner.MustExec(`insert into t values (1,"Matt"),(2,"Anne")`)
	tkB.MustQuery(`select /*+ USE_INDEX(t, PRIMARY) */ a from t`).Check(testkit.Rows("2"))
	tkB.MustQuery(`select * from t`).Check(testkit.Rows("2 Anne"))
	domNonOwner.Reload()
	require.Equal(t, verCurr, domNonOwner.InfoSchema().SchemaMetaVersion())
	tkB.MustQuery(`show create table t`).Check(testkit.Rows("" +
		"t CREATE TABLE `t` (\n" +
		"  `a` int(11) NOT NULL,\n" +
		"  `b` varchar(255) COLLATE utf8mb4_0900_ai_ci DEFAULT NULL,\n" +
		"  PRIMARY KEY (`a`) /*T![clustered_index] NONCLUSTERED */ /*T![global_index] GLOBAL */\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin\n" +
		"PARTITION BY RANGE COLUMNS(`b`)\n" +
		"(PARTITION `p0` VALUES LESS THAN ('m'),\n" +
		" PARTITION `p1` VALUES LESS THAN ('p'))"))
	tkB.MustQuery(ddlJobsSQL).CheckAt([]int{4, 11}, [][]any{{"public", "synced"}})
	tkB.MustQuery(`select /*+ USE_INDEX(t, PRIMARY) */ a from t`).Check(testkit.Rows("1", "2"))
	tkB.MustQuery(`select * from t`).Sort().Check(testkit.Rows("1 Matt", "2 Anne"))
}

func TestMultiSchemaVerDropPartition(t *testing.T) {
	// Last domain will be the DDLOwner
	distCtx := testkit.NewDistExecutionContextWithLease(t, 2, 15*time.Second)
	store := distCtx.Store
	dom1 := distCtx.GetDomain(0)
	dom2 := distCtx.GetDomain(1)
	defer func() {
		dom1.Close()
		dom2.Close()
		store.Close()
	}()

	se1, err := session.CreateSessionWithDomain(store, dom1)
	require.NoError(t, err)
	se2, err := session.CreateSessionWithDomain(store, dom2)
	require.NoError(t, err)

	// Session on non DDL owner domain (~ TiDB Server)
	tk1 := testkit.NewTestKitWithSession(t, store, se1)
	tk1.MustExec(`use test`)
	tk1.MustExec(`set @@global.tidb_enable_global_index = 1`)
	tk1.MustExec(`set @@session.tidb_enable_global_index = 1`)
	// Session on DDL owner domain (~ TiDB Server), used for concurrent DDL
	tk2 := testkit.NewTestKitWithSession(t, store, se2)
	tk2.MustExec(`use test`)
	tk2.MustExec(`set @@session.tidb_enable_global_index = 1`)
	// Session on DDL owner domain (~ TiDB Server), used for queries
	tk3 := testkit.NewTestKitWithSession(t, store, se2)
	tk3.MustExec(`use test`)
	// The DDL Owner will be the last created domain, so use tk2.

	// TODO: Create data so that each state can update and delete at least on row from each of the previous states.
	// Why does it have to be from each of the previous states?'
	// Is it not enough if it is rows from previous state/loop?
	// Also test that it can find every row, through every index as well as full table scan.
	// And check what happens on duplicate key for insert and delete.

	// for SchemaVer - 1 test that:
	// 1. all operations works for rows not touched by any of SchemaVer - 1 NOR SchemaVer (all should be fully visible)
	// 2. all operations works for rows touched by SchemaVer - 1 (All should be fully visible)
	// 3. all operations works for rows touched by SchemaVer (All should be fully visible, but expect some edge cases during deletion of old global index entries)
	// for SchemaVer test that
	// 1. all operations works for rows not touched by any of SchemaVer - 1 NOR SchemaVer (all should be fully visible)
	// 2. all operations works for rows touched by SchemaVer (All should be fully visible)
	// 3. all operations works for rows touched by SchemaVer - 1 (If part of dropped partition, it should not be seen, and not create duplicate errors)

	// Special handling to check what happens during delete reorg, maybe have a before and after, as well as a specific failpoint?

	states := []string{"start", "delete only", "delete reorganization", "done"}

	offsetP1 := 1000000
	// TODO: Use several maps, one per index?
	// Or maybe just one per state?
	// Columns:
	// id - int
	// secondary id (in reverse) - used for a second unique index
	// partition
	// inserted at state
	// update history - Should be the full permutation of higher level states (i.e. from zero, some, to all)
	/*
		rows := make(map[string][][]string)
		rowNr := 1
		rows[states[0]] = [][]string{{"1", "999999", "p0", "0", ""}, {"1000001", "1999999", "p0", "0", ""}}
		rowNr++
		//             update state->curr state->start (false)
		rangesUpdate := make(map[int]map[int]map[bool]int)
		rangesDelete := make(map[int]map[int]map[bool]int)
		for i := range states[1:] {
			for j := 0; j <= i; j++ {
				rowsToAdd := len(rows[states[j]])
				rangesUpdate[j][i][false] = rowNr
				s := strconv.Itoa(i)
				for r := 0; r < rowsToAdd; r++ {
					// For Update
					rows[states[j]] = append(rows[states[j]], []string{strconv.Itoa(rowNr), strconv.Itoa(offsetP1 - rowNr), "p0", s, ""})
					rows[states[j]] = append(rows[states[j]], []string{strconv.Itoa(rowNr + offsetP1), strconv.Itoa(2*offsetP1 - rowNr), "p1", s, ""})
					rowNr++
				}
				rangesUpdate[j][i][true] = rowNr
				rangesDelete[j][i][false] = rowNr
				for r := 0; r < rowsToAdd; r++ {
					// For Delete
					rows[states[j]] = append(rows[states[j]], []string{strconv.Itoa(rowNr), strconv.Itoa(offsetP1 - rowNr), "p0", s, ""})
					rows[states[j]] = append(rows[states[j]], []string{strconv.Itoa(rowNr + offsetP1), strconv.Itoa(2*offsetP1 - rowNr), "p1", s, ""})
					rowNr++
				}
				rangesDelete[j][i][true] = rowNr
			}
		}

	*/
	tk1.MustExec(`create table t (id int unsigned primary key nonclustered, b int, part varchar(10) not null, state int not null, history text)`)
	dom1.Reload()
	dom2.Reload()
	dbRows := make(map[int][]string)
	lastRowID := 8
	for i := 1; i <= lastRowID; i++ {
		dbRows[i] = []string{strconv.Itoa(i), strconv.Itoa(offsetP1 - i), "p0", "-1", ""}
	}
	for i := range dbRows {
		a, b, c, d, e := dbRows[i][0], dbRows[i][1], dbRows[i][2], dbRows[i][3], dbRows[i][4]
		tk1.MustExec(fmt.Sprintf(`insert into t values (%s, %s, '%s', %s, '%s')`, a, b, c, d, e))
	}
	//tk1.MustQuery(`select * from t`).Sort().Check(testkit.Rows())
	for i := range states {
		require.GreaterOrEqual(t, i, 0)
		// TODO: Add initial rows, i.e. state -1
		// SchemaVer - 1: A
		// SchemaVer:     B
		getIds := 8
		keys := make([]int, 0, getIds)
		for key := range dbRows {
			keys = append(keys, key)
			if len(keys) >= getIds {
				break
			}
		}
		// B:
		lastRowIDForB := lastRowID
		lastRowID = step1(tk1, dbRows, keys[:4], i, offsetP1, lastRowID)
		// A:
		lastRowIDForA := lastRowID
		lastRowID = step1(tk2, dbRows, keys[4:], i, offsetP1, lastRowID)
		// B:
		lastRowID = step2(tk1, dbRows, keys[4:], i, offsetP1, lastRowIDForA+1, lastRowID)
		// A:
		lastRowID = step2(tk2, dbRows, keys[:4], i, offsetP1, lastRowIDForB+1, lastRowID)
		// same as B
	}
	//tk1.MustQuery(`select * from t`).Sort().Check(testkit.Rows())
	// First iteration, which rows needs to exists 'before':
	// 3 to update, 1 to delete X 2 = 8 rows
	// then 3 new ones - 1 delete X 2 = 10 rows
	// 1 new, 2 delete X 2 = 8 rows => Can we just keep 8 rows?
	// Would flipping the order between stage changes matter for the test?
}

func step1(tk *testkit.TestKit, dbRows map[int][]string, keys []int, state int, offset int, lastRowID int) int {
	// update at least three row from 'before'
	for j := 0; j < 3; j++ {
		// TODO: Should the updates also affect the indexes?
		r := dbRows[keys[j]]
		hist := fmt.Sprintf(":Update(%d:%d:%d)", state, j, lastRowID)
		r[4] += hist
		tk.MustExec(fmt.Sprintf(`update t set history = concat(history,'%s') where id = %d`, hist, keys[j]))
	}
	// delete at least one row from 'before'
	tk.MustExec(fmt.Sprintf(`delete from t where id = %d`, keys[3]))
	delete(dbRows, keys[3])
	// insert at least three rows (one to keep untouched, one to update, one to delete)
	lastRowID++
	dbRows[lastRowID] = []string{strconv.Itoa(lastRowID), strconv.Itoa(offset - lastRowID), "p0", strconv.Itoa(state), ""}
	a, b, c, d, e := dbRows[lastRowID][0], dbRows[lastRowID][1], dbRows[lastRowID][2], dbRows[lastRowID][3], dbRows[lastRowID][4]
	tk.MustExec(fmt.Sprintf(`insert into t values (%s, %s, '%s', %s, '%s')`, a, b, c, d, e))
	lastRowID++
	dbRows[lastRowID] = []string{strconv.Itoa(lastRowID), strconv.Itoa(offset - lastRowID), "p0", strconv.Itoa(state), ""}
	a, b, c, d, e = dbRows[lastRowID][0], dbRows[lastRowID][1], dbRows[lastRowID][2], dbRows[lastRowID][3], dbRows[lastRowID][4]
	tk.MustExec(fmt.Sprintf(`insert into t values (%s, %s, '%s', %s, '%s')`, a, b, c, d, e))
	lastRowID++
	dbRows[lastRowID] = []string{strconv.Itoa(lastRowID), strconv.Itoa(offset - lastRowID), "p0", strconv.Itoa(state), ""}
	a, b, c, d, e = dbRows[lastRowID][0], dbRows[lastRowID][1], dbRows[lastRowID][2], dbRows[lastRowID][3], dbRows[lastRowID][4]
	tk.MustExec(fmt.Sprintf(`insert into t values (%s, %s, '%s', %s, '%s')`, a, b, c, d, e))
	// Check for rows, including test insert and update for duplicate key!!!
	return lastRowID
}

func step2(tk *testkit.TestKit, dbRows map[int][]string, keys []int, state int, offset int, startID int, lastRowID int) int {
	// insert one row
	lastRowID++
	dbRows[lastRowID] = []string{strconv.Itoa(lastRowID), strconv.Itoa(offset - lastRowID), "p0", strconv.Itoa(state), ""}
	a, b, c, d, e := dbRows[lastRowID][0], dbRows[lastRowID][1], dbRows[lastRowID][2], dbRows[lastRowID][3], dbRows[lastRowID][4]
	tk.MustExec(fmt.Sprintf(`insert into t values (%s, %s, '%s', %s, '%s')`, a, b, c, d, e))
	// update one row that A inserted
	r := dbRows[startID]
	hist := fmt.Sprintf(":Update(%d:%d:%d:%d)", state, 4, startID, lastRowID)
	r[4] += hist
	tk.MustExec(fmt.Sprintf(`update t set history = concat(history,'%s') where id = %d`, hist, startID))
	startID++
	// update one row that A updated
	r = dbRows[keys[0]]
	hist = fmt.Sprintf(":Update(%d:%d:%d:%d)", state, 4, startID, lastRowID)
	r[4] += hist
	tk.MustExec(fmt.Sprintf(`update t set history = concat(history,'%s') where id = %d`, hist, keys[0]))
	// delete one row that A inserted
	tk.MustExec(fmt.Sprintf(`delete from t where id = %d`, startID))
	delete(dbRows, startID)
	startID++
	// delete one row that A updated
	tk.MustExec(fmt.Sprintf(`delete from t where id = %d`, keys[1]))
	delete(dbRows, keys[1])
	// Check for the row A deleted
	return lastRowID
}
