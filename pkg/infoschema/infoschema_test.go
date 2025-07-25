// Copyright 2015 PingCAP, Inc.
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

package infoschema_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/ddl/placement"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/infoschema/internal"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/meta/metadef"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/testutil"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/stretchr/testify/require"
)

func TestBasic(t *testing.T) {
	re := internal.CreateAutoIDRequirement(t)
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	dbName := ast.NewCIStr("Test")
	tbName := ast.NewCIStr("T")
	colName := ast.NewCIStr("A")
	idxName := ast.NewCIStr("idx")
	noexist := ast.NewCIStr("noexist")

	colID, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	colInfo := &model.ColumnInfo{
		ID:        colID,
		Name:      colName,
		Offset:    0,
		FieldType: *types.NewFieldType(mysql.TypeLonglong),
		State:     model.StatePublic,
	}

	idxInfo := &model.IndexInfo{
		Name:  idxName,
		Table: tbName,
		Columns: []*model.IndexColumn{
			{
				Name:   colName,
				Offset: 0,
				Length: 10,
			},
		},
		Unique:  true,
		Primary: true,
		State:   model.StatePublic,
	}

	tbID, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	tblInfo := &model.TableInfo{
		ID:      tbID,
		Name:    tbName,
		Columns: []*model.ColumnInfo{colInfo},
		Indices: []*model.IndexInfo{idxInfo},
		State:   model.StatePublic,
	}

	dbID, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	dbInfo := &model.DBInfo{
		ID:    dbID,
		Name:  dbName,
		State: model.StatePublic,
	}
	dbInfo.Deprecated.Tables = []*model.TableInfo{tblInfo}
	tblInfo.DBID = dbInfo.ID

	dbInfos := []*model.DBInfo{dbInfo}
	internal.AddDB(t, re.Store(), dbInfo)
	internal.AddTable(t, re.Store(), dbInfo.ID, tblInfo)

	builder := infoschema.NewBuilder(re, nil, infoschema.NewData(), vardef.SchemaCacheSize.Load() > 0)
	err = builder.InitWithDBInfos(dbInfos, nil, nil, 1)
	require.NoError(t, err)

	txn, err := re.Store().Begin()
	require.NoError(t, err)
	checkApplyCreateNonExistsSchemaDoesNotPanic(t, txn, builder)
	checkApplyCreateNonExistsTableDoesNotPanic(t, txn, builder, dbID)
	err = txn.Rollback()
	require.NoError(t, err)

	ver, err := re.Store().CurrentVersion(kv.GlobalTxnScope)
	require.NoError(t, err)
	is := builder.Build(ver.Ver)

	schemaNames := infoschema.AllSchemaNames(is)
	require.Len(t, schemaNames, 3)
	require.True(t, testutil.CompareUnorderedStringSlice(schemaNames, []string{metadef.InformationSchemaName.O, metadef.MetricSchemaName.O, "Test"}))

	schemas := is.AllSchemaNames()
	require.Len(t, schemas, 3)

	require.True(t, is.SchemaExists(dbName))
	require.False(t, is.SchemaExists(noexist))

	schema, ok := is.SchemaByID(dbID)
	require.True(t, ok)
	require.NotNil(t, schema)

	schema, ok = is.SchemaByID(tbID)
	require.False(t, ok)
	require.Nil(t, schema)

	schema, ok = is.SchemaByName(dbName)
	require.True(t, ok)
	require.NotNil(t, schema)

	schema, ok = is.SchemaByName(noexist)
	require.False(t, ok)
	require.Nil(t, schema)

	schema, ok = infoschema.SchemaByTable(is, tblInfo)
	require.True(t, ok)
	require.NotNil(t, schema)

	b, err := json.Marshal(tblInfo)
	require.NoError(t, err)
	tblUnmarshal := &model.TableInfo{}
	err = json.Unmarshal(b, tblUnmarshal)
	require.NoError(t, err)
	schema2, ok := infoschema.SchemaByTable(is, tblUnmarshal)
	require.True(t, ok)
	require.Equal(t, schema, schema2)

	noexistTblInfo := &model.TableInfo{ID: 12345, Name: tblInfo.Name}
	schema, ok = infoschema.SchemaByTable(is, noexistTblInfo)
	require.False(t, ok)
	require.Nil(t, schema)

	require.True(t, is.TableExists(dbName, tbName))
	require.False(t, is.TableExists(dbName, noexist))
	require.False(t, infoschema.TableIsView(is, dbName, tbName))
	require.False(t, infoschema.TableIsSequence(is, dbName, tbName))

	tb, ok := is.TableByID(context.Background(), tbID)
	require.True(t, ok)
	require.NotNil(t, tb)

	gotTblInfo, ok := is.TableInfoByID(tbID)
	require.True(t, ok)
	require.Same(t, tb.Meta(), gotTblInfo)

	tb, ok = is.TableByID(context.Background(), dbID)
	require.False(t, ok)
	require.Nil(t, tb)

	gotTblInfo, ok = is.TableInfoByID(dbID)
	require.False(t, ok)
	require.Nil(t, gotTblInfo)

	tb, ok = is.TableByID(context.Background(), -12345)
	require.False(t, ok)
	require.Nil(t, tb)

	gotTblInfo, ok = is.TableInfoByID(-12345)
	require.False(t, ok)
	require.Nil(t, gotTblInfo)

	tb, err = is.TableByName(context.Background(), dbName, tbName)
	require.NoError(t, err)
	require.NotNil(t, tb)

	gotTblInfo, err = is.TableInfoByName(dbName, tbName)
	require.NoError(t, err)
	require.Same(t, tb.Meta(), gotTblInfo)

	_, err = is.TableByName(context.Background(), dbName, noexist)
	require.Error(t, err)

	gotTblInfo, err = is.TableInfoByName(dbName, noexist)
	require.Error(t, err)
	require.Nil(t, gotTblInfo)

	// negative id should always be seen as not exists
	tb, ok = is.TableByID(context.Background(), -1)
	require.False(t, ok)
	require.Nil(t, tb)
	schema, ok = is.SchemaByID(-1)
	require.False(t, ok)
	require.Nil(t, schema)
	gotTblInfo, ok = is.TableInfoByID(-1)
	require.Nil(t, gotTblInfo)
	require.False(t, ok)

	tblInfos, err := is.SchemaTableInfos(context.Background(), dbName)
	require.NoError(t, err)
	require.Len(t, tblInfos, 1)
	tbl, ok := is.TableByID(context.Background(), tblInfos[0].ID)
	require.True(t, ok)
	require.Equal(t, tbl.Meta(), tblInfos[0]) // Equal but not Same

	tblInfos, err = is.SchemaTableInfos(context.Background(), noexist)
	require.NoError(t, err)
	require.Len(t, tblInfos, 0)

	// Make sure partitions table exists
	tb, err = is.TableByName(context.Background(), ast.NewCIStr("information_schema"), ast.NewCIStr("partitions"))
	require.NoError(t, err)
	require.NotNil(t, tb)

	require.NoError(t, err)
	txn, err = re.Store().Begin()
	require.NoError(t, err)
	_, err = builder.ApplyDiff(meta.NewMutator(txn), &model.SchemaDiff{
		Type:        model.ActionRenameTable,
		SchemaID:    dbID,
		TableID:     tbID,
		OldSchemaID: dbID,
		Version:     is.SchemaMetaVersion() + 1,
	})
	require.NoError(t, err)
	err = txn.Rollback()
	require.NoError(t, err)
	is = builder.Build(math.MaxUint64)
	schema, ok = is.SchemaByID(dbID)
	require.True(t, ok)
	tbls, err := is.SchemaTableInfos(context.Background(), schema.Name)
	require.NoError(t, err)
	require.Equal(t, 1, len(tbls))

	// Test SchemaNameByTableID
	tests := []struct {
		name       string
		tableID    int64
		wantSchema ast.CIStr
		wantOK     bool
	}{
		{
			name:       "valid table ID",
			tableID:    tbID,
			wantSchema: dbName,
			wantOK:     true,
		},
		{
			name:       "non-existent table ID",
			tableID:    tbID + 1,
			wantSchema: ast.CIStr{},
			wantOK:     false,
		},
		{
			name:       "invalid table ID (negative)",
			tableID:    -1,
			wantSchema: ast.CIStr{},
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotItem, gotOK := is.TableItemByID(tt.tableID)
			require.Equal(t, tt.wantOK, gotOK)
			require.Equal(t, tt.wantSchema, gotItem.DBName)
		})
	}
}

func TestMockInfoSchema(t *testing.T) {
	tblID := int64(1234)
	tblName := ast.NewCIStr("tbl_m")
	tableInfo := &model.TableInfo{
		ID:    tblID,
		Name:  tblName,
		State: model.StatePublic,
	}
	colInfo := &model.ColumnInfo{
		State:     model.StatePublic,
		Offset:    0,
		Name:      ast.NewCIStr("h"),
		FieldType: *types.NewFieldType(mysql.TypeLong),
		ID:        1,
	}
	tableInfo.Columns = []*model.ColumnInfo{colInfo}
	is := infoschema.MockInfoSchema([]*model.TableInfo{tableInfo})
	tbl, ok := is.TableByID(context.Background(), tblID)
	require.True(t, ok)
	require.Equal(t, tblName, tbl.Meta().Name)
	require.Equal(t, colInfo, tbl.Cols()[0].ColumnInfo)
}

func checkApplyCreateNonExistsSchemaDoesNotPanic(t *testing.T, txn kv.Transaction, builder *infoschema.Builder) {
	m := meta.NewMutator(txn)
	_, err := builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateSchema, SchemaID: 999, Version: 1})
	require.True(t, infoschema.ErrDatabaseNotExists.Equal(err))
}

func checkApplyCreateNonExistsTableDoesNotPanic(t *testing.T, txn kv.Transaction, builder *infoschema.Builder, dbID int64) {
	m := meta.NewMutator(txn)
	_, err := builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: dbID, TableID: 999, Version: 1})
	require.True(t, infoschema.ErrTableNotExists.Equal(err))
}

// TestInfoTables makes sure that all tables of information_schema could be found in infoschema handle.
func TestInfoTables(t *testing.T) {
	re := internal.CreateAutoIDRequirement(t)

	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	builder := infoschema.NewBuilder(re, nil, infoschema.NewData(), vardef.SchemaCacheSize.Load() > 0)
	err := builder.InitWithDBInfos(nil, nil, nil, 0)
	require.NoError(t, err)
	is := builder.Build(math.MaxUint64)

	infoTables := []string{
		"CHARACTER_SETS",
		"COLLATIONS",
		"COLLATION_CHARACTER_SET_APPLICABILITY",
		"COLUMNS",
		"COLUMN_PRIVILEGES",
		"DEADLOCKS",
		"ENGINES",
		"EVENTS",
		"FILES",
		"KEY_COLUMN_USAGE",
		"OPTIMIZER_TRACE",
		"PARAMETERS",
		"PARTITIONS",
		"PLACEMENT_POLICIES",
		"PLUGINS",
		"PROCESSLIST",
		"PROFILING",
		"REFERENTIAL_CONSTRAINTS",
		"RESOURCE_GROUPS",
		"ROUTINES",
		"SCHEMATA",
		"SCHEMA_PRIVILEGES",
		"STATISTICS",
		"TABLES",
		"TABLESPACES",
		"TABLE_CONSTRAINTS",
		"TABLE_PRIVILEGES",
		"TIDB_TRX",
		"TRIGGERS",
		"TRX_SUMMARY",
		"USER_PRIVILEGES",
		"VIEWS",
	}
	for _, tbl := range infoTables {
		tb, err1 := is.TableByName(context.Background(), metadef.InformationSchemaName, ast.NewCIStr(tbl))
		require.Nil(t, err1)
		require.NotNil(t, tb)
	}
}

func TestBuildSchemaWithGlobalTemporaryTable(t *testing.T) {
	re := internal.CreateAutoIDRequirement(t)
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	dbInfo := &model.DBInfo{
		ID:    1,
		Name:  ast.NewCIStr("test"),
		State: model.StatePublic,
	}
	dbInfo.Deprecated.Tables = []*model.TableInfo{}
	dbInfos := []*model.DBInfo{dbInfo}
	data := infoschema.NewData()
	builder := infoschema.NewBuilder(re, nil, data, vardef.SchemaCacheSize.Load() > 0)
	err := builder.InitWithDBInfos(dbInfos, nil, nil, 1)
	require.NoError(t, err)
	is := builder.Build(math.MaxUint64)
	require.False(t, is.HasTemporaryTable())
	db, ok := is.SchemaByName(ast.NewCIStr("test"))
	require.True(t, ok)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	err = kv.RunInNewTxn(ctx, re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMutator(txn).CreateDatabase(dbInfo)
		require.NoError(t, err)
		return errors.Trace(err)
	})
	require.NoError(t, err)

	doChange := func(changes ...func(m *meta.Mutator, builder *infoschema.Builder)) infoschema.InfoSchema {
		ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
		curIs := is
		err := kv.RunInNewTxn(ctx, re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
			m := meta.NewMutator(txn)
			for _, change := range changes {
				builder = infoschema.NewBuilder(re, nil, data, vardef.SchemaCacheSize.Load() > 0)
				err := builder.InitWithOldInfoSchema(curIs)
				require.NoError(t, err)
				change(m, builder)
				curIs = builder.Build(math.MaxUint64)
			}
			return nil
		})
		require.NoError(t, err)
		return curIs
	}

	createGlobalTemporaryTableChange := func(tblID int64) func(m *meta.Mutator, builder *infoschema.Builder) {
		return func(m *meta.Mutator, builder *infoschema.Builder) {
			err := m.CreateTableOrView(db.ID, &model.TableInfo{
				ID:            tblID,
				TempTableType: model.TempTableGlobal,
				State:         model.StatePublic,
			})
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: db.ID, TableID: tblID, Version: 1})
			require.NoError(t, err)
		}
	}

	createNormalTableChange := func(tblID int64) func(m *meta.Mutator, builder *infoschema.Builder) {
		return func(m *meta.Mutator, builder *infoschema.Builder) {
			err := m.CreateTableOrView(db.ID, &model.TableInfo{
				ID:    tblID,
				State: model.StatePublic,
			})
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: db.ID, TableID: tblID, Version: 1})
			require.NoError(t, err)
		}
	}

	dropTableChange := func(tblID int64) func(m *meta.Mutator, builder *infoschema.Builder) {
		return func(m *meta.Mutator, builder *infoschema.Builder) {
			err := m.DropTableOrView(db.ID, tblID)
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionDropTable, SchemaID: db.ID, TableID: tblID, Version: 1})
			require.NoError(t, err)
		}
	}

	truncateGlobalTemporaryTableChange := func(tblID, newTblID int64) func(m *meta.Mutator, builder *infoschema.Builder) {
		return func(m *meta.Mutator, builder *infoschema.Builder) {
			err := m.DropTableOrView(db.ID, tblID)
			require.NoError(t, err)

			err = m.CreateTableOrView(db.ID, &model.TableInfo{
				ID:            newTblID,
				TempTableType: model.TempTableGlobal,
				State:         model.StatePublic,
			})
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionTruncateTable, SchemaID: db.ID, OldTableID: tblID, TableID: newTblID, Version: 1})
			require.NoError(t, err)
		}
	}

	alterTableChange := func(tblID int64) func(m *meta.Mutator, builder *infoschema.Builder) {
		return func(m *meta.Mutator, builder *infoschema.Builder) {
			_, err := builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionAddColumn, SchemaID: db.ID, TableID: tblID, Version: 1})
			require.NoError(t, err)
		}
	}

	// create table
	tbID, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	newIS := doChange(
		createGlobalTemporaryTableChange(tbID),
	)
	require.True(t, newIS.HasTemporaryTable())

	// full load
	data = infoschema.NewData()
	newDB, ok := newIS.SchemaByName(ast.NewCIStr("test"))
	tblInfos, err := newIS.SchemaTableInfos(context.Background(), newDB.Name)
	require.NoError(t, err)
	newDB.Deprecated.Tables = tblInfos
	require.True(t, ok)
	builder = infoschema.NewBuilder(re, nil, data, vardef.SchemaCacheSize.Load() > 0)
	err = builder.InitWithDBInfos([]*model.DBInfo{newDB}, newIS.AllPlacementPolicies(), newIS.AllResourceGroups(), newIS.SchemaMetaVersion())
	require.NoError(t, err)
	require.True(t, builder.Build(math.MaxUint64).HasTemporaryTable())

	// create and then drop
	tbID, err = internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	require.False(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		dropTableChange(tbID),
	).HasTemporaryTable())

	// create and then alter
	tbID, err = internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		alterTableChange(tbID),
	).HasTemporaryTable())

	// create and truncate
	tbID, err = internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	newTbID, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		truncateGlobalTemporaryTableChange(tbID, newTbID),
	).HasTemporaryTable())

	// create two and drop one
	tbID, err = internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	tbID2, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		createGlobalTemporaryTableChange(tbID2),
		dropTableChange(tbID),
	).HasTemporaryTable())

	// create temporary and then create normal
	tbID, err = internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	tbID2, err = internal.GenGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		createNormalTableChange(tbID2),
	).HasTemporaryTable())
}

func TestBuildBundle(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("drop placement policy if exists p1")
	tk.MustExec("drop placement policy if exists p2")
	tk.MustExec("create placement policy p1 followers=1")
	tk.MustExec("create placement policy p2 followers=2")
	tk.MustExec(`create table t1(a int primary key) placement policy p1 partition by range(a) (
		partition p1 values less than (10) placement policy p2,
		partition p2 values less than (20)
	)`)
	tk.MustExec("create table t2(a int)")
	defer func() {
		tk.MustExec("drop table if exists t1, t2")
		tk.MustExec("drop placement policy if exists p1")
		tk.MustExec("drop placement policy if exists p2")
	}()

	is := domain.GetDomain(tk.Session()).InfoSchema()
	db, ok := is.SchemaByName(ast.NewCIStr("test"))
	require.True(t, ok)

	tbl1, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t1"))
	require.NoError(t, err)

	tbl2, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t2"))
	require.NoError(t, err)

	var p1 model.PartitionDefinition
	for _, par := range tbl1.Meta().Partition.Definitions {
		if par.Name.L == "p1" {
			p1 = par
			break
		}
	}
	require.NotNil(t, p1)

	var tb1Bundle, p1Bundle *placement.Bundle

	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	require.NoError(t, kv.RunInNewTxn(ctx, store, false, func(ctx context.Context, txn kv.Transaction) (err error) {
		m := meta.NewMutator(txn)
		tb1Bundle, err = placement.NewTableBundle(m, tbl1.Meta())
		require.NoError(t, err)
		require.NotNil(t, tb1Bundle)

		p1Bundle, err = placement.NewPartitionBundle(m, p1)
		require.NoError(t, err)
		require.NotNil(t, p1Bundle)
		return
	}))

	assertBundle := func(checkIS infoschema.InfoSchema, id int64, expected *placement.Bundle) {
		actual, ok := checkIS.PlacementBundleByPhysicalTableID(id)
		if expected == nil {
			require.False(t, ok)
			return
		}

		expectedJSON, err := json.Marshal(expected)
		require.NoError(t, err)
		actualJSON, err := json.Marshal(actual)
		require.NoError(t, err)
		require.Equal(t, string(expectedJSON), string(actualJSON))
	}

	assertBundle(is, tbl1.Meta().ID, tb1Bundle)
	assertBundle(is, tbl2.Meta().ID, nil)
	assertBundle(is, p1.ID, p1Bundle)

	if len(db.Deprecated.Tables) == 0 {
		var err error
		db.Deprecated.Tables, err = is.SchemaTableInfos(context.Background(), db.Name)
		require.NoError(t, err)
	}
	builder := infoschema.NewBuilder(dom, nil, infoschema.NewData(), vardef.SchemaCacheSize.Load() > 0)
	err = builder.InitWithDBInfos([]*model.DBInfo{db}, is.AllPlacementPolicies(), is.AllResourceGroups(), is.SchemaMetaVersion())
	require.NoError(t, err)
	is2 := builder.Build(math.MaxUint64)
	assertBundle(is2, tbl1.Meta().ID, tb1Bundle)
	assertBundle(is2, tbl2.Meta().ID, nil)
	assertBundle(is2, p1.ID, p1Bundle)
}

func TestWithRefillOption(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("set @@global.tidb_schema_cache_size = 512 * 1024 * 1024")

	tk.MustExec("create table t1 (id int)")
	tk.MustQuery("select * from t1").Check(testkit.Rows())
	is := dom.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t1"))
	require.NoError(t, err)
	tblInfo := tbl.Meta()
	ok, v2 := infoschema.IsV2(is)
	require.True(t, ok)

	hit := true
	miss := false
	testCases := []struct {
		OP     string
		ctx    context.Context
		expect bool
	}{
		{"TableByName", context.Background(), hit},
		{"TableByName", infoschema.WithRefillOption(context.Background(), true), hit},
		{"TableByName", infoschema.WithRefillOption(context.Background(), false), miss},
		{"TableByID", context.Background(), miss},
		{"TableByID", infoschema.WithRefillOption(context.Background(), true), hit},
		{"TableByID", infoschema.WithRefillOption(context.Background(), false), miss},
	}

	for i, testCase := range testCases {
		// Mock t1 schema cache been evicted.
		v2.EvictTable(ast.NewCIStr("test"), ast.NewCIStr("t1"))

		// Test the API
		switch testCase.OP {
		case "TableByID":
			_, found := is.TableByID(testCase.ctx, tblInfo.ID)
			require.True(t, found)
		case "TableByName":
			_, err := is.TableByName(testCase.ctx, ast.NewCIStr("test"), ast.NewCIStr("t1"))
			require.NoError(t, err)
		}

		got := v2.HasCache(tblInfo.ID, is.SchemaMetaVersion())
		require.Equal(t, testCase.expect, got, fmt.Sprintf("case %d failed", i))
	}
}

func TestLocalTemporaryTables(t *testing.T) {
	re := internal.CreateAutoIDRequirement(t)
	var err error
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	createNewSchemaInfo := func(schemaName string) *model.DBInfo {
		schemaID, err := internal.GenGlobalID(re.Store())
		require.NoError(t, err)
		return &model.DBInfo{
			ID:    schemaID,
			Name:  ast.NewCIStr(schemaName),
			State: model.StatePublic,
		}
	}

	createNewTable := func(schemaID int64, tbName string) table.Table {
		colID, err := internal.GenGlobalID(re.Store())
		require.NoError(t, err)

		colInfo := &model.ColumnInfo{
			ID:        colID,
			Name:      ast.NewCIStr("col1"),
			Offset:    0,
			FieldType: *types.NewFieldType(mysql.TypeLonglong),
			State:     model.StatePublic,
		}

		tbID, err := internal.GenGlobalID(re.Store())
		require.NoError(t, err)

		tblInfo := &model.TableInfo{
			ID:      tbID,
			Name:    ast.NewCIStr(tbName),
			Columns: []*model.ColumnInfo{colInfo},
			Indices: []*model.IndexInfo{},
			State:   model.StatePublic,
			DBID:    schemaID,
		}

		allocs := autoid.NewAllocatorsFromTblInfo(re, schemaID, tblInfo)
		tbl, err := table.TableFromMeta(allocs, tblInfo)
		require.NoError(t, err)

		return tbl
	}

	assertTableByName := func(sc *infoschema.SessionTables, schemaName, tableName string, schema *model.DBInfo, tb table.Table) {
		got, ok := sc.TableByName(context.Background(), ast.NewCIStr(schemaName), ast.NewCIStr(tableName))
		if tb == nil {
			require.Nil(t, schema)
			require.False(t, ok)
			require.Nil(t, got)
		} else {
			require.NotNil(t, schema)
			require.True(t, ok)
			require.Equal(t, tb, got)
		}
	}

	assertTableExists := func(sc *infoschema.SessionTables, schemaName, tableName string, exists bool) {
		got := sc.TableExists(ast.NewCIStr(schemaName), ast.NewCIStr(tableName))
		require.Equal(t, exists, got)
	}

	assertTableByID := func(sc *infoschema.SessionTables, tbID int64, schema *model.DBInfo, tb table.Table) {
		got, ok := sc.TableByID(tbID)
		if tb == nil {
			require.Nil(t, schema)
			require.False(t, ok)
			require.Nil(t, got)
		} else {
			require.NotNil(t, schema)
			require.True(t, ok)
			require.Equal(t, tb, got)
		}
	}

	assertSchemaByTable := func(sc *infoschema.SessionTables, db *model.DBInfo, tb *model.TableInfo) {
		got, ok := sc.SchemaByID(tb.DBID)
		if db == nil {
			require.Nil(t, got)
			require.False(t, ok)
		} else {
			require.NotNil(t, got)
			require.Equal(t, db.Name.L, got.Name.L)
			require.True(t, ok)
		}
	}

	sc := infoschema.NewSessionTables()
	db1 := createNewSchemaInfo("db1")
	tb11 := createNewTable(db1.ID, "tb1")
	tb12 := createNewTable(db1.ID, "Tb2")
	tb13 := createNewTable(db1.ID, "tb3")

	// db1b has the same name with db1
	db1b := createNewSchemaInfo("db1b")
	tb15 := createNewTable(db1b.ID, "tb5")
	tb16 := createNewTable(db1b.ID, "tb6")
	tb17 := createNewTable(db1b.ID, "tb7")

	db2 := createNewSchemaInfo("db2")
	tb21 := createNewTable(db2.ID, "tb1")
	tb22 := createNewTable(db2.ID, "TB2")
	tb24 := createNewTable(db2.ID, "tb4")

	prepareTables := []struct {
		db *model.DBInfo
		tb table.Table
	}{
		{db1, tb11}, {db1, tb12}, {db1, tb13},
		{db1b, tb15}, {db1b, tb16}, {db1b, tb17},
		{db2, tb21}, {db2, tb22}, {db2, tb24},
	}

	for _, p := range prepareTables {
		err = sc.AddTable(p.db, p.tb)
		require.NoError(t, err)
	}

	// test exist tables
	for _, p := range prepareTables {
		dbName := p.db.Name
		tbName := p.tb.Meta().Name

		assertTableByName(sc, dbName.O, tbName.O, p.db, p.tb)
		assertTableByName(sc, dbName.L, tbName.L, p.db, p.tb)
		assertTableByName(
			sc,
			strings.ToUpper(dbName.L[:1])+dbName.L[1:],
			strings.ToUpper(tbName.L[:1])+tbName.L[1:],
			p.db, p.tb,
		)

		assertTableExists(sc, dbName.O, tbName.O, true)
		assertTableExists(sc, dbName.L, tbName.L, true)
		assertTableExists(
			sc,
			strings.ToUpper(dbName.L[:1])+dbName.L[1:],
			strings.ToUpper(tbName.L[:1])+tbName.L[1:],
			true,
		)

		assertTableByID(sc, p.tb.Meta().ID, p.db, p.tb)
		assertSchemaByTable(sc, p.db, p.tb.Meta())
	}

	// test add dup table
	err = sc.AddTable(db1, tb11)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	err = sc.AddTable(db1b, tb15)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	err = sc.AddTable(db1b, tb11)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	db1c := createNewSchemaInfo("db1")
	err = sc.AddTable(db1c, createNewTable(db1c.ID, "tb1"))
	require.True(t, infoschema.ErrTableExists.Equal(err))
	err = sc.AddTable(db1b, tb11)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	tb11.Meta().DBID = 0 // SchemaByTable will get incorrect result if not reset here.

	// failed add has no effect
	assertTableByName(sc, db1.Name.L, tb11.Meta().Name.L, db1, tb11)

	// delete some tables
	require.True(t, sc.RemoveTable(ast.NewCIStr("db1"), ast.NewCIStr("tb1")))
	require.True(t, sc.RemoveTable(ast.NewCIStr("Db2"), ast.NewCIStr("tB2")))
	tb22.Meta().DBID = 0 // SchemaByTable will get incorrect result if not reset here.
	require.False(t, sc.RemoveTable(ast.NewCIStr("db1"), ast.NewCIStr("tbx")))
	require.False(t, sc.RemoveTable(ast.NewCIStr("dbx"), ast.NewCIStr("tbx")))

	// test non exist tables by name
	for _, c := range []struct{ dbName, tbName string }{
		{"db1", "tb1"}, {"db1", "tb4"}, {"db1", "tbx"},
		{"db2", "tb2"}, {"db2", "tb3"}, {"db2", "tbx"},
		{"dbx", "tb1"},
	} {
		assertTableByName(sc, c.dbName, c.tbName, nil, nil)
		assertTableExists(sc, c.dbName, c.tbName, false)
	}

	// test non exist tables by id
	nonExistID, err := internal.GenGlobalID(re.Store())
	require.NoError(t, err)

	for _, id := range []int64{nonExistID, tb11.Meta().ID, tb22.Meta().ID} {
		assertTableByID(sc, id, nil, nil)
	}

	// test non exist table schemaByTable
	assertSchemaByTable(sc, nil, tb11.Meta())
	assertSchemaByTable(sc, nil, tb22.Meta())

	// test SessionExtendedInfoSchema
	dbTest := createNewSchemaInfo("test")
	tmpTbTestA := createNewTable(dbTest.ID, "tba")
	normalTbTestA := createNewTable(dbTest.ID, "tba")
	normalTbTestB := createNewTable(dbTest.ID, "tbb")
	normalTbTestC := createNewTable(db1.ID, "tbc")

	is := &infoschema.SessionExtendedInfoSchema{
		InfoSchema:           infoschema.MockInfoSchema([]*model.TableInfo{normalTbTestA.Meta(), normalTbTestB.Meta()}),
		LocalTemporaryTables: sc,
	}

	err = sc.AddTable(dbTest, tmpTbTestA)
	require.NoError(t, err)

	// test TableByName
	tbl, err := is.TableByName(context.Background(), dbTest.Name, normalTbTestA.Meta().Name)
	require.NoError(t, err)
	require.Equal(t, tmpTbTestA, tbl)
	gotTblInfo, err := is.TableInfoByName(dbTest.Name, normalTbTestA.Meta().Name)
	require.NoError(t, err)
	require.Same(t, tmpTbTestA.Meta(), gotTblInfo)

	tbl, err = is.TableByName(context.Background(), dbTest.Name, normalTbTestB.Meta().Name)
	require.NoError(t, err)
	require.Equal(t, normalTbTestB.Meta(), tbl.Meta())
	gotTblInfo, err = is.TableInfoByName(dbTest.Name, normalTbTestB.Meta().Name)
	require.NoError(t, err)
	require.Same(t, tbl.Meta(), gotTblInfo)

	tbl, err = is.TableByName(context.Background(), db1.Name, tb11.Meta().Name)
	require.True(t, infoschema.ErrTableNotExists.Equal(err))
	require.Nil(t, tbl)
	gotTblInfo, err = is.TableInfoByName(dbTest.Name, tb11.Meta().Name)
	require.True(t, infoschema.ErrTableNotExists.Equal(err))
	require.Nil(t, gotTblInfo)

	tbl, err = is.TableByName(context.Background(), db1.Name, tb12.Meta().Name)
	require.NoError(t, err)
	require.Equal(t, tb12, tbl)
	gotTblInfo, err = is.TableInfoByName(db1.Name, tb12.Meta().Name)
	require.NoError(t, err)
	require.Same(t, tbl.Meta(), gotTblInfo)

	// test TableByID
	tbl, ok := is.TableByID(context.Background(), normalTbTestA.Meta().ID)
	require.True(t, ok)
	require.Equal(t, normalTbTestA.Meta(), tbl.Meta())
	gotTblInfo, ok = is.TableInfoByID(normalTbTestA.Meta().ID)
	require.True(t, ok)
	require.Same(t, tbl.Meta(), gotTblInfo)

	tbl, ok = is.TableByID(context.Background(), normalTbTestB.Meta().ID)
	require.True(t, ok)
	require.Equal(t, normalTbTestB.Meta(), tbl.Meta())
	gotTblInfo, ok = is.TableInfoByID(normalTbTestB.Meta().ID)
	require.True(t, ok)
	require.Same(t, tbl.Meta(), gotTblInfo)

	tbl, ok = is.TableByID(context.Background(), tmpTbTestA.Meta().ID)
	require.True(t, ok)
	require.Equal(t, tmpTbTestA, tbl)
	gotTblInfo, ok = is.TableInfoByID(tmpTbTestA.Meta().ID)
	require.True(t, ok)
	require.Same(t, tbl.Meta(), gotTblInfo)

	tbl, ok = is.TableByID(context.Background(), tb12.Meta().ID)
	require.True(t, ok)
	require.Equal(t, tb12, tbl)
	gotTblInfo, ok = is.TableInfoByID(tb12.Meta().ID)
	require.True(t, ok)
	require.Same(t, tbl.Meta(), gotTblInfo)

	tbl, ok = is.TableByID(context.Background(), 1234567)
	require.False(t, ok)
	require.Nil(t, tbl)
	gotTblInfo, ok = is.TableInfoByID(1234567)
	require.False(t, ok)
	require.Nil(t, gotTblInfo)

	// test SchemaByTable
	info, ok := is.SchemaByID(normalTbTestA.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, dbTest.Name.L, info.Name.L)
	info, ok = is.SchemaByID(normalTbTestB.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, dbTest.Name.L, info.Name.L)
	info, ok = is.SchemaByID(tmpTbTestA.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, dbTest.Name.L, info.Name.L)
	// SchemaByTable also returns DBInfo when the schema is not in the infoSchema but the table is an existing tmp table.
	info, ok = is.SchemaByID(tb12.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, db1.Name.L, info.Name.L)
	// SchemaByTable returns nil when the schema is not in the infoSchema and the table is an non-existing normal table.
	normalTbTestC.Meta().DBID = 0 // normalTbTestC is not added to any db, reset the DBID to avoid misuse
	info, ok = is.SchemaByID(normalTbTestC.Meta().DBID)
	require.False(t, ok)
	require.Nil(t, info)
	// SchemaByTable returns nil when the schema is not in the infoSchema and the table is an non-existing tmp table.
	info, ok = is.SchemaByID(tb22.Meta().DBID)
	require.False(t, ok)
	require.Nil(t, info)

	// negative id should always be seen as not exists
	tbl, ok = is.TableByID(context.Background(), -1)
	require.False(t, ok)
	require.Nil(t, tbl)
	info, ok = is.SchemaByID(-1)
	require.False(t, ok)
	require.Nil(t, info)
}

// TestInfoSchemaCreateTableLike tests the table's column ID and index ID for memory database.
func TestInfoSchemaCreateTableLike(t *testing.T) {
	store := testkit.CreateMockStore(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table vi like information_schema.variables_info;")
	tk.MustExec("alter table vi modify min_value varchar(32);")
	tk.MustExec("create table u like metrics_schema.up;")
	tk.MustExec("alter table u modify job int;")
	tk.MustExec("create table so like performance_schema.setup_objects;")
	tk.MustExec("alter table so modify object_name int;")

	tk.MustExec("create table t1 like information_schema.variables_info;")
	tk.MustExec("alter table t1 add column c varchar(32);")
	is := domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t1"))
	require.NoError(t, err)
	tblInfo := tbl.Meta()
	require.Equal(t, tblInfo.Columns[8].Name.O, "c")
	require.Equal(t, tblInfo.Columns[8].ID, int64(9))
	tk.MustExec("alter table t1 add index idx(c);")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err = is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t1"))
	require.NoError(t, err)
	tblInfo = tbl.Meta()
	require.Equal(t, tblInfo.Indices[0].Name.O, "idx")
	require.Equal(t, tblInfo.Indices[0].ID, int64(1))

	// metrics_schema
	tk.MustExec("create table t2 like metrics_schema.up;")
	tk.MustExec("alter table t2 add column c varchar(32);")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err = is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t2"))
	require.NoError(t, err)
	tblInfo = tbl.Meta()
	require.Equal(t, tblInfo.Columns[4].Name.O, "c")
	require.Equal(t, tblInfo.Columns[4].ID, int64(5))
	tk.MustExec("alter table t2 add index idx(c);")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err = is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t2"))
	require.NoError(t, err)
	tblInfo = tbl.Meta()
	require.Equal(t, tblInfo.Indices[0].Name.O, "idx")
	require.Equal(t, tblInfo.Indices[0].ID, int64(1))
}

func TestEnableInfoSchemaV2(t *testing.T) {
	t.Skip("This feature is not enabled yet")
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	// Test the @@tidb_enable_infoschema_v2 variable.
	tk.MustQuery("select @@tidb_schema_cache_size").Check(testkit.Rows("0"))
	tk.MustQuery("select @@global.tidb_schema_cache_size").Check(testkit.Rows("0"))
	require.Equal(t, vardef.SchemaCacheSize.Load(), uint64(0))

	// Modify it.
	tk.MustExec("set @@global.tidb_schema_cache_size = 1073741824")
	tk.MustQuery("select @@global.tidb_schema_cache_size").Check(testkit.Rows("1073741824"))
	tk.MustQuery("select @@tidb_schema_cache_size").Check(testkit.Rows("1073741824"))
	require.Equal(t, vardef.SchemaCacheSize.Load(), uint64(1073741824))

	tk.MustExec("use test")
	tk.MustExec("create table v2 (id int)")

	// Check the InfoSchema used is V2.
	is := domain.GetDomain(tk.Session()).InfoSchema()
	isV2, _ := infoschema.IsV2(is)
	require.True(t, isV2)

	// Execute some basic operations under infoschema v2.
	tk.MustQuery("show tables").Check(testkit.Rows("v2"))
	tk.MustExec("create table pt (id int) partition by range (id) (partition p0 values less than (10), partition p1 values less than (20))")
	tk.MustExec("truncate table v2")
	tk.MustExec("truncate table pt")
	tk.MustExec("alter table pt truncate partition p0")
	tk.MustExec("alter table pt drop partition p0")
	tk.MustExec("drop table v2")
	tk.MustExec("create table v1 (id int)")

	// Change infoschema back to v1 and check again.
	tk.MustExec("set @@global.tidb_schema_cache_size = 0")
	tk.MustQuery("select @@global.tidb_schema_cache_size").Check(testkit.Rows("0"))
	require.Equal(t, vardef.SchemaCacheSize.Load(), uint64(0))

	tk.MustExec("drop table v1")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	isV2, _ = infoschema.IsV2(is)
	require.False(t, isV2)
}

type infoschemaTestContext struct {
	// only test one db.
	dbInfo *model.DBInfo
	t      testing.TB
	re     autoid.Requirement
	ctx    context.Context
	data   *infoschema.Data
	is     infoschema.InfoSchema
}

func (tc *infoschemaTestContext) createSchema() {
	dbInfo := internal.MockDBInfo(tc.t, tc.re.Store(), "test")
	internal.AddDB(tc.t, tc.re.Store(), dbInfo)
	tc.dbInfo = dbInfo
	// init infoschema
	builder := infoschema.NewBuilder(tc.re, nil, tc.data, vardef.SchemaCacheSize.Load() > 0)
	err := builder.InitWithDBInfos([]*model.DBInfo{}, nil, nil, 1)
	require.NoError(tc.t, err)
	tc.is = builder.Build(math.MaxUint64)
}

func (tc *infoschemaTestContext) runCreateSchema() {
	// create schema
	tc.createSchema()
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionCreateSchema, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		dbInfo, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, dbInfo.Name, tc.dbInfo.Name)
	})
}

func (tc *infoschemaTestContext) runDropSchema() {
	// create schema
	tc.runCreateSchema()
	// drop schema
	internal.DropDB(tc.t, tc.re.Store(), tc.dbInfo)
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionDropSchema, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		_, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.False(tc.t, ok)
	})
}

func (tc *infoschemaTestContext) runRecoverSchema() {
	tc.runDropSchema()
	// recover schema
	internal.AddDB(tc.t, tc.re.Store(), tc.dbInfo)
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionRecoverSchema, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		dbInfo, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, dbInfo.Name, tc.dbInfo.Name)
	})
}

func (tc *infoschemaTestContext) runCreateTable(tblName string) int64 {
	if tc.dbInfo == nil {
		tc.runCreateSchema()
	}
	// create table
	tblInfo := internal.MockTableInfo(tc.t, tc.re.Store(), tblName)
	internal.AddTable(tc.t, tc.re.Store(), tc.dbInfo.ID, tblInfo)

	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: tc.dbInfo.ID, TableID: tblInfo.ID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(context.Background(), tblInfo.ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, tbl.Meta().Name.O, tblName)
	})
	return tblInfo.ID
}

func (tc *infoschemaTestContext) runCreateTables(tblNames []string) {
	if tc.dbInfo == nil {
		tc.runCreateSchema()
	}
	diff := model.SchemaDiff{Type: model.ActionCreateTables, SchemaID: tc.dbInfo.ID}
	diff.AffectedOpts = make([]*model.AffectedOption, len(tblNames))
	for i, tblName := range tblNames {
		tblInfo := internal.MockTableInfo(tc.t, tc.re.Store(), tblName)
		internal.AddTable(tc.t, tc.re.Store(), tc.dbInfo.ID, tblInfo)
		diff.AffectedOpts[i] = &model.AffectedOption{
			SchemaID: tc.dbInfo.ID,
			TableID:  tblInfo.ID,
		}
	}

	tc.applyDiffAndCheck(&diff, func(tc *infoschemaTestContext) {
		for i, opt := range diff.AffectedOpts {
			tbl, ok := tc.is.TableByID(context.Background(), opt.TableID)
			require.True(tc.t, ok)
			require.Equal(tc.t, tbl.Meta().Name.O, tblNames[i])
		}
	})
}

func (tc *infoschemaTestContext) runDropTable(tblName string) {
	// createTable
	tblID := tc.runCreateTable(tblName)

	// dropTable
	internal.DropTable(tc.t, tc.re.Store(), tc.dbInfo, tblID, tblName)
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionDropTable, SchemaID: tc.dbInfo.ID, TableID: tblID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(context.Background(), tblID)
		require.False(tc.t, ok)
		require.Nil(tc.t, tbl)
	})
}

func (tc *infoschemaTestContext) runModifyTable(tblName string, tp model.ActionType) {
	switch tp {
	case model.ActionAddColumn:
		tc.runAddColumn(tblName)
	case model.ActionModifyColumn:
		tc.runModifyColumn(tblName)
	default:
		return
	}
}

func (tc *infoschemaTestContext) runAddColumn(tblName string) {
	tbl, err := tc.is.TableByName(context.Background(), tc.dbInfo.Name, ast.NewCIStr(tblName))
	require.NoError(tc.t, err)

	tc.addColumn(tbl.Meta())
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionAddColumn, SchemaID: tc.dbInfo.ID, TableID: tbl.Meta().ID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(context.Background(), tbl.Meta().ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, 2, len(tbl.Cols()))
	})
}

func (tc *infoschemaTestContext) addColumn(tblInfo *model.TableInfo) {
	colName := ast.NewCIStr("b")
	colID, err := internal.GenGlobalID(tc.re.Store())
	require.NoError(tc.t, err)
	colInfo := &model.ColumnInfo{
		ID:        colID,
		Name:      colName,
		Offset:    1,
		FieldType: *types.NewFieldType(mysql.TypeLonglong),
		State:     model.StatePublic,
	}

	tblInfo.Columns = append(tblInfo.Columns, colInfo)
	err = kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMutator(txn).UpdateTable(tc.dbInfo.ID, tblInfo)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
}

func (tc *infoschemaTestContext) runModifyColumn(tblName string) {
	tbl, err := tc.is.TableByName(context.Background(), tc.dbInfo.Name, ast.NewCIStr(tblName))
	require.NoError(tc.t, err)

	tc.modifyColumn(tbl.Meta())
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionModifyColumn, SchemaID: tc.dbInfo.ID, TableID: tbl.Meta().ID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(context.Background(), tbl.Meta().ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, "test", tbl.Cols()[0].Comment)
	})
}

func (tc *infoschemaTestContext) modifyColumn(tblInfo *model.TableInfo) {
	columnInfo := tblInfo.Columns
	columnInfo[0].Comment = "test"

	err := kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMutator(txn).UpdateTable(tc.dbInfo.ID, tblInfo)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
}

func (tc *infoschemaTestContext) runModifySchemaCharsetAndCollate(charset, collate string) {
	tc.dbInfo.Charset = charset
	tc.dbInfo.Collate = collate
	internal.UpdateDB(tc.t, tc.re.Store(), tc.dbInfo)
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionModifySchemaCharsetAndCollate, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		schema, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, charset, schema.Charset)
		require.Equal(tc.t, collate, schema.Collate)
	})
}

func (tc *infoschemaTestContext) runModifySchemaDefaultPlacement(policy *model.PolicyRefInfo) {
	tc.dbInfo.PlacementPolicyRef = policy
	internal.UpdateDB(tc.t, tc.re.Store(), tc.dbInfo)
	tc.applyDiffAndCheck(&model.SchemaDiff{Type: model.ActionModifySchemaDefaultPlacement, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		schema, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, policy, schema.PlacementPolicyRef)
	})
}

func (tc *infoschemaTestContext) applyDiffAndCheck(diff *model.SchemaDiff, checkFn func(tc *infoschemaTestContext)) {
	txn, err := tc.re.Store().Begin()
	require.NoError(tc.t, err)

	builder := infoschema.NewBuilder(tc.re, nil, tc.data, vardef.SchemaCacheSize.Load() > 0)
	err = builder.InitWithOldInfoSchema(tc.is)
	require.NoError(tc.t, err)
	// applyDiff
	_, err = builder.ApplyDiff(meta.NewMutator(txn), diff)
	require.NoError(tc.t, err)
	tc.is = builder.Build(math.MaxUint64)
	checkFn(tc)
}

func (tc *infoschemaTestContext) clear() {
	tc.dbInfo = nil
	tc.is = nil
}

func TestApplyDiff(t *testing.T) {
	re := internal.CreateAutoIDRequirement(t)
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	for i := range 2 {
		if i == 0 {
			// enable infoschema v2.
			vardef.SchemaCacheSize.Store(1000000)
		}

		tc := &infoschemaTestContext{
			t:    t,
			re:   re,
			ctx:  kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL),
			data: infoschema.NewData(),
		}
		tc.runRecoverSchema()
		tc.clear()
		tc.runCreateSchema()
		tc.clear()
		tc.runDropSchema()
		tc.clear()
		tc.runCreateTable("test")
		tc.clear()
		tc.runDropTable("test")
		tc.clear()

		tc.runCreateTable("test")
		tc.runModifyTable("test", model.ActionAddColumn)
		tc.runModifyTable("test", model.ActionModifyColumn)

		tc.runModifySchemaCharsetAndCollate("utf8mb4", "utf8mb4_general_ci")
		tc.runModifySchemaCharsetAndCollate("utf8", "utf8_unicode_ci")
		tc.runModifySchemaDefaultPlacement(&model.PolicyRefInfo{
			Name: ast.NewCIStr("test"),
		})
		tc.runCreateTables([]string{"test1", "test2"})
	}
	// TODO(ywqzzy): check all actions.
}
