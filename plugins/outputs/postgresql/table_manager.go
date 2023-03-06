package postgresql

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v4"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/sqltemplate"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

// This is an arbitrary constant value shared between multiple telegraf processes used for locking schema updates.
const schemaAdvisoryLockID int64 = 5705450890675909945

type tableState struct {
	name    string
	columns map[string]utils.Column
	sync.RWMutex
}

type TableManager struct {
	*Postgresql

	// map[tableName]map[columnName]utils.Column
	tables      map[string]*tableState
	tablesMutex sync.Mutex
}

// NewTableManager returns an instance of the tables.Manager interface
// that can handle checking and updating the state of tables in the PG database.
func NewTableManager(postgresql *Postgresql) *TableManager {
	return &TableManager{
		Postgresql: postgresql,
		tables:     make(map[string]*tableState),
	}
}

// ClearTableCache clear the table structure cache.
func (tm *TableManager) ClearTableCache() {
	tm.tablesMutex.Lock()
	for _, tbl := range tm.tables {
		tbl.Lock()
		tbl.columns = nil
		tbl.Unlock()
	}
	tm.tablesMutex.Unlock()

	if tm.tagsCache != nil {
		tm.tagsCache.Clear()
	}
}

func (tm *TableManager) table(name string) *tableState {
	tm.tablesMutex.Lock()
	tbl := tm.tables[name]
	if tbl == nil {
		tbl = &tableState{name: name}
		tm.tables[name] = tbl
	}
	tm.tablesMutex.Unlock()
	return tbl
}

// MatchSource scans through the metrics, determining what columns are needed for inserting, and ensuring the DB schema matches.
//
// If the schema does not match, and schema updates are disabled:
// If a field missing from the DB, the field is omitted.
// If a tag is missing from the DB, the metric is dropped.
func (tm *TableManager) MatchSource(ctx context.Context, db dbh, rowSource *TableSource) error {
	metricTable := tm.table(rowSource.Name())
	var tagTable *tableState
	if tm.TagsAsForeignKeys {
		tagTable = tm.table(metricTable.name + tm.TagTableSuffix)

		missingCols, err := tm.EnsureStructure(
			ctx,
			db,
			tagTable,
			rowSource.TagTableColumns(),
			tm.TagTableCreateTemplates,
			tm.TagTableAddColumnTemplates,
			metricTable,
			tagTable,
		)
		if err != nil {
			return err
		}

		if len(missingCols) > 0 {
			colDefs := make([]string, len(missingCols))
			for i, col := range missingCols {
				if err := rowSource.DropColumn(col); err != nil {
					return fmt.Errorf("metric/table mismatch: Unable to omit field/column from \"%s\": %w", tagTable.name, err)
				}
				colDefs[i] = col.Name + " " + col.Type
			}
			tm.Logger.Errorf("table '%s' is missing tag columns (dropping metrics): %s",
				tagTable.name,
				strings.Join(colDefs, ", "))
		}
	}

	missingCols, err := tm.EnsureStructure(
		ctx,
		db,
		metricTable,
		rowSource.MetricTableColumns(),
		tm.CreateTemplates,
		tm.AddColumnTemplates,
		metricTable,
		tagTable,
	)
	if err != nil {
		return err
	}

	if len(missingCols) > 0 {
		colDefs := make([]string, len(missingCols))
		for i, col := range missingCols {
			if err := rowSource.DropColumn(col); err != nil {
				return fmt.Errorf("metric/table mismatch: Unable to omit field/column from \"%s\": %w", metricTable.name, err)
			}
			colDefs[i] = col.Name + " " + col.Type
		}
		tm.Logger.Errorf("table \"%s\" is missing columns (omitting fields): %s",
			metricTable.name,
			strings.Join(colDefs, ", "))
	}

	return nil
}

// EnsureStructure ensures that the table identified by tableName contains the provided columns.
//
// createTemplates and addColumnTemplates are the templates which are executed in the event of table create or alter
// (respectively).
// metricsTableName and tagsTableName are passed to the templates.
//
// If the table cannot be modified, the returned column list is the columns which are missing from the table.
//nolint:revive
func (tm *TableManager) EnsureStructure(
	ctx context.Context,
	db dbh,
	tbl *tableState,
	columns []utils.Column,
	createTemplates []*sqltemplate.Template,
	addColumnsTemplates []*sqltemplate.Template,
	metricsTable *tableState,
	tagsTable *tableState,
) ([]utils.Column, error) {
	// Sort so that:
	//   * When we create/alter the table the columns are in a sane order (telegraf gives us the fields in random order)
	//   * When we display errors about missing columns, the order is also sane, and consistent
	utils.ColumnList(columns).Sort()

	// rlock, read, runlock, wlock, read, read_db, wlock_db, read_db, write_db, wunlock_db, wunlock

	// rlock
	tbl.RLock()
	// read
	currCols := tbl.columns
	// runlock
	tbl.RUnlock()
	missingCols := diffMissingColumns(currCols, columns)
	if len(missingCols) == 0 {
		return nil, nil
	}

	// wlock
	// We also need to lock the other table as it may be referenced by a template.
	// To prevent deadlock, the metric & tag table must always be locked in the same order: 1) Tag, 2) Metric
	if tbl == tagsTable {
		tagsTable.Lock()
		defer tagsTable.Unlock()

		metricsTable.RLock()
		defer metricsTable.RUnlock()
	} else {
		if tagsTable != nil {
			tagsTable.RLock()
			defer tagsTable.RUnlock()
		}

		metricsTable.Lock()
		defer metricsTable.Unlock()
	}

	// read
	currCols = tbl.columns
	missingCols = diffMissingColumns(currCols, columns)
	if len(missingCols) == 0 {
		return nil, nil
	}

	// read_db
	var err error
	if currCols, err = tm.getColumns(ctx, db, tbl.name); err != nil {
		return nil, err
	}
	tbl.columns = currCols
	missingCols = diffMissingColumns(currCols, columns)
	if len(missingCols) == 0 {
		tbl.columns = currCols
		return nil, nil
	}

	if len(currCols) == 0 && len(createTemplates) == 0 {
		// can't create
		return missingCols, nil
	}
	if len(currCols) != 0 && len(addColumnsTemplates) == 0 {
		// can't add
		return missingCols, nil
	}

	// wlock_db
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// It's possible to have multiple telegraf processes, in which we can't ensure they all lock tables in the same
	// order. So to prevent possible deadlocks, we have to have a single lock for all schema modifications.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaAdvisoryLockID); err != nil {
		return nil, err
	}

	// read_db
	if currCols, err = tm.getColumns(ctx, tx, tbl.name); err != nil {
		return nil, err
	}
	tbl.columns = currCols
	if currCols != nil {
		missingCols = diffMissingColumns(currCols, columns)
		if len(missingCols) == 0 {
			return nil, nil
		}
	}

	// write_db
	var tmpls []*sqltemplate.Template
	if len(currCols) == 0 {
		tmpls = createTemplates
	} else {
		tmpls = addColumnsTemplates
	}
	if err := tm.update(ctx, tx, tbl, tmpls, missingCols, metricsTable, tagsTable); err != nil {
		return nil, err
	}

	if currCols, err = tm.getColumns(ctx, tx, tbl.name); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	tbl.columns = currCols

	// wunlock_db (deferred)
	// wunlock (deferred)

	return nil, nil
}

func (tm *TableManager) getColumns(ctx context.Context, db dbh, name string) (map[string]utils.Column, error) {
	rows, err := db.Query(ctx, `
		SELECT
			column_name,
			CASE WHEN data_type='USER-DEFINED' THEN udt_name ELSE data_type END,
			col_description(format('%I.%I', table_schema, table_name)::regclass::oid, ordinal_position)
		FROM information_schema.columns
		WHERE table_schema = $1 and table_name = $2`, tm.Schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]utils.Column)
	for rows.Next() {
		var colName, colType string
		desc := new(string)
		err := rows.Scan(&colName, &colType, &desc)
		if err != nil {
			return nil, err
		}

		role := utils.FieldColType
		switch colName {
		case timeColumnName:
			role = utils.TimeColType
		case tagIDColumnName:
			role = utils.TagsIDColType
		case tagsJSONColumnName:
			role = utils.TagColType
		case fieldsJSONColumnName:
			role = utils.FieldColType
		default:
			// We don't want to monopolize the column comment (preventing user from storing other information there), so just look at the first word
			if desc != nil {
				descWords := strings.Split(*desc, " ")
				if descWords[0] == "tag" {
					role = utils.TagColType
				}
			}
		}

		cols[colName] = utils.Column{
			Name: colName,
			Type: colType,
			Role: role,
		}
	}

	return cols, rows.Err()
}

//nolint:revive
func (tm *TableManager) update(ctx context.Context,
	tx pgx.Tx,
	state *tableState,
	tmpls []*sqltemplate.Template,
	missingCols []utils.Column,
	metricsTable *tableState,
	tagsTable *tableState,
) error {
	tmplTable := sqltemplate.NewTable(tm.Schema, state.name, colMapToSlice(state.columns))
	metricsTmplTable := sqltemplate.NewTable(tm.Schema, metricsTable.name, colMapToSlice(metricsTable.columns))
	var tagsTmplTable *sqltemplate.Table
	if tagsTable != nil {
		tagsTmplTable = sqltemplate.NewTable(tm.Schema, tagsTable.name, colMapToSlice(tagsTable.columns))
	} else {
		tagsTmplTable = sqltemplate.NewTable("", "", nil)
	}

	for _, tmpl := range tmpls {
		sql, err := tmpl.Render(tmplTable, missingCols, metricsTmplTable, tagsTmplTable)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("executing `%s`: %w", sql, err)
		}
	}

	// We need to be able to determine the role of the column when reading the structure back (because of the templates).
	// For some columns we can determine this by the column name (time, tag_id, etc). However tags and fields can have any
	// name, and look the same. So we add a comment to tag columns, and through process of elimination what remains are
	// field columns.
	for _, col := range missingCols {
		if col.Role != utils.TagColType {
			continue
		}
		stmt := fmt.Sprintf("COMMENT ON COLUMN %s.%s IS 'tag'",
			tmplTable.String(), sqltemplate.QuoteIdentifier(col.Name))
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("setting column role comment: %s", err)
		}
	}

	return nil
}

// diffMissingColumns filters srcColumns to the ones not present in dbColumns.
func diffMissingColumns(dbColumns map[string]utils.Column, srcColumns []utils.Column) []utils.Column {
	if len(dbColumns) == 0 {
		return srcColumns
	}

	var missingColumns []utils.Column
	for _, srcCol := range srcColumns {
		if _, ok := dbColumns[srcCol.Name]; !ok {
			missingColumns = append(missingColumns, srcCol)
			continue
		}
	}
	return missingColumns
}

func colMapToSlice(colMap map[string]utils.Column) []utils.Column {
	if colMap == nil {
		return nil
	}
	cols := make([]utils.Column, 0, len(colMap))
	for _, col := range colMap {
		cols = append(cols, col)
	}
	return cols
}