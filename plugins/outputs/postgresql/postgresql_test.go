package postgresql

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

func timeout(t *testing.T, dur time.Duration) {
	timer := time.AfterFunc(dur, func() {
		t.Errorf("Test timed out after %s", dur)
		t.FailNow()
	})
	t.Cleanup(func() { timer.Stop() })
}

type Log struct {
	level  pgx.LogLevel
	format string
	args   []interface{}
}

func (l Log) String() string {
	return fmt.Sprintf("%s: "+l.format, append([]interface{}{l.level}, l.args...)...)
}

// LogAccumulator is a log collector that satisfies telegraf.Logger.
type LogAccumulator struct {
	logs []Log
	cond *sync.Cond
	tb   testing.TB
}

func NewLogAccumulator(tb testing.TB) *LogAccumulator {
	return &LogAccumulator{
		cond: sync.NewCond(&sync.Mutex{}),
		tb:   tb,
	}
}

func (la *LogAccumulator) append(level pgx.LogLevel, format string, args []interface{}) {
	la.cond.L.Lock()
	log := Log{level, format, args}
	la.logs = append(la.logs, log)
	s := log.String()
	la.tb.Helper()
	la.tb.Log(s)
	la.cond.Broadcast()
	la.cond.L.Unlock()
}

func (la *LogAccumulator) WaitLen(n int) []Log {
	la.cond.L.Lock()
	defer la.cond.L.Unlock()
	for len(la.logs) < n {
		la.cond.Wait()
	}
	return la.logs[:]
}

// Waits for a specific query log from pgx to show up.
func (la *LogAccumulator) WaitFor(f func(l Log) bool, waitCommit bool) {
	la.cond.L.Lock()
	defer la.cond.L.Unlock()
	i := 0
	var commitPid uint32
	for {
		for ; i < len(la.logs); i++ {
			log := la.logs[i]
			if commitPid == 0 {
				if f(log) {
					if !waitCommit {
						return
					}
					commitPid = log.args[1].(MSI)["pid"].(uint32)
				}
			} else {
				if len(log.args) < 2 {
					continue
				}
				data, ok := log.args[1].(MSI)
				if !ok || data["pid"] != commitPid {
					continue
				}
				if log.args[0] == "Exec" && data["sql"] == "commit" {
					return
				} else if log.args[0] == "Exec" && data["sql"] == "rollback" {
					// transaction aborted, start looking for another match
					commitPid = 0
				} else if log.level == pgx.LogLevelError {
					commitPid = 0
				}
			}
		}
		la.cond.Wait()
	}
}

func (la *LogAccumulator) WaitForQuery(str string, waitCommit bool) {
	la.WaitFor(func(log Log) bool {
		return log.format == "PG %s - %+v" &&
			(log.args[0].(string) == "Query" || log.args[0].(string) == "Exec") &&
			strings.Contains(log.args[1].(MSI)["sql"].(string), str)
	}, waitCommit)
}

func (la *LogAccumulator) WaitForCopy(tableName string, waitCommit bool) {
	la.WaitFor(func(log Log) bool {
		return log.format == "PG %s - %+v" &&
			log.args[0].(string) == "CopyFrom" &&
			log.args[1].(MSI)["tableName"].(pgx.Identifier)[1] == tableName
	}, waitCommit)
}

// Clear any stored logs.
// Do not run this while any WaitFor* operations are in progress.
func (la *LogAccumulator) Clear() {
	la.cond.L.Lock()
	if len(la.logs) > 0 {
		la.logs = nil
	}
	la.cond.L.Unlock()
}

//func (la *LogAccumulator) Chan() <-chan Log {
//	ch := make(chan Log)
//	rch := (<-chan Log)(ch)
//	go func() {
//		cond := la.cond
//		logs := &la.logs
//		runtime.SetFinalizer(rch, func(_ <-chan string) {
//			cond.L.Lock()
//			logs = nil
//			cond.Broadcast()
//			cond.L.Unlock()
//		})
//		la = nil
//		i := 0
//		cond.L.Lock()
//		for {
//			if logs == nil {
//				break
//			}
//			if i == len(*logs) {
//				cond.Wait()
//				continue
//			}
//			log := (*logs)[i]
//			i++
//			cond.L.Unlock()
//			ch <- log
//			cond.L.Lock()
//		}
//		cond.L.Unlock()
//	}()
//	return rch
//}

func (la *LogAccumulator) Logs() []Log {
	la.cond.L.Lock()
	defer la.cond.L.Unlock()
	return la.logs[:]
}

func (la *LogAccumulator) Errorf(format string, args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelError, format, args)
}

func (la *LogAccumulator) Error(args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelError, "%v", args)
}

func (la *LogAccumulator) Debugf(format string, args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelDebug, format, args)
}

func (la *LogAccumulator) Debug(args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelDebug, "%v", args)
}

func (la *LogAccumulator) Warnf(format string, args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelWarn, format, args)
}

func (la *LogAccumulator) Warn(args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelWarn, "%v", args)
}

func (la *LogAccumulator) Infof(format string, args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelInfo, format, args)
}

func (la *LogAccumulator) Info(args ...interface{}) {
	la.tb.Helper()
	la.append(pgx.LogLevelInfo, "%v", args)
}

var ctx context.Context

func TestMain(m *testing.M) {
	if os.Getenv("PGHOST") == "" && os.Getenv("PGHOSTADDR") == "" && os.Getenv("PGPORT") == "" {
		// User has not specified a server, use the default, which is the one defined by docker-compose.yml at the top of the repo.
		os.Setenv("PGHOST", "127.0.0.1")
		os.Setenv("PGPORT", "5433")
		os.Setenv("PGUSER", "postgres")
	}

	ctx = context.Background()
	if err := prepareDatabase("telegraf"); err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing database: %s\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func prepareDatabase(name string) error {
	db, err := pgx.Connect(ctx, "")
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, "CREATE DATABASE "+name)
	return err
}

type PostgresqlTest struct {
	Postgresql
	Logger *LogAccumulator
}

func newPostgresqlTest(tb testing.TB) *PostgresqlTest {
	p := newPostgresql()
	logger := NewLogAccumulator(tb)
	p.Logger = logger
	pt := &PostgresqlTest{Postgresql: *p}
	pt.Logger = logger
	pt.Connection = "database=telegraf"
	pt.LogLevel = "debug"
	return pt
}

func TestPostgresqlConnect(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())
	assert.EqualValues(t, 1, p.db.Stat().MaxConns())
	p.Close()

	p = newPostgresqlTest(t)
	p.Connection += " pool_max_conns=2"
	require.NoError(t, p.Connect())
	assert.EqualValues(t, 2, p.db.Stat().MaxConns())
	p.Close()
}

func TestDBConnectedHook(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))

	c, _ := p.db.Acquire(ctx)
	c.Conn().Close(ctx)
	c.Release()

	_, err := p.db.Exec(ctx, "SELECT 1")
	require.NoError(t, err)

	assert.Empty(t, p.tableManager.table(t.Name()).Columns())
}

func newMetric(
	t *testing.T,
	suffix string,
	tags map[string]string,
	fields map[string]interface{},
) telegraf.Metric {
	return metric.New(t.Name()+suffix, tags, fields, time.Now())
}

type MSS = map[string]string
type MSI = map[string]interface{}

func dbTableDump(t *testing.T, db *pgxpool.Pool, suffix string) []MSI {
	rows, err := db.Query(ctx, "SELECT * FROM "+pgx.Identifier{t.Name() + suffix}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()

	var dump []MSI
	for rows.Next() {
		msi := MSI{}
		vals, err := rows.Values()
		require.NoError(t, err)
		for i, fd := range rows.FieldDescriptions() {
			msi[string(fd.Name)] = vals[i]
		}
		dump = append(dump, msi)
	}
	require.NoError(t, rows.Err())
	return dump
}

func TestWrite_sequential(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 1}),
		newMetric(t, "_b", MSS{}, MSI{"v": 2}),
		newMetric(t, "_a", MSS{}, MSI{"v": 3}),
	}
	require.NoError(t, p.Write(metrics))

	dumpA := dbTableDump(t, p.db, "_a")
	dumpB := dbTableDump(t, p.db, "_b")

	if assert.Len(t, dumpA, 2) {
		assert.EqualValues(t, 1, dumpA[0]["v"])
		assert.EqualValues(t, 3, dumpA[1]["v"])
	}
	if assert.Len(t, dumpB, 1) {
		assert.EqualValues(t, 2, dumpB[0]["v"])
	}
}

func TestWrite_concurrent(t *testing.T) {
	p := newPostgresqlTest(t)
	p.Connection += " pool_max_conns=3"
	require.NoError(t, p.Connect())

	// Write a metric so it creates a table we can lock.
	metrics := []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))
	p.Logger.WaitForCopy(t.Name()+"_a", true)
	// clear so that the WaitForCopy calls below don't pick up this one
	p.Logger.Clear()

	// Lock the table so that we ensure the writes hangs and the plugin has to open another connection.
	tx, err := p.db.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "LOCK TABLE "+utils.QuoteIdent(t.Name()+"_a"))
	require.NoError(t, err)

	metrics = []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 2}),
	}
	require.NoError(t, p.Write(metrics))

	// Note, there is technically a possible race here, where it doesn't try to insert into _a until after _b. However
	// this should be practically impossible, and trying to engineer a solution to account for it would be even more
	// complex than we already are.

	metrics = []telegraf.Metric{
		newMetric(t, "_b", MSS{}, MSI{"v": 3}),
	}
	require.NoError(t, p.Write(metrics))

	p.Logger.WaitForCopy(t.Name()+"_b", true)
	// release the lock on table _a
	tx.Rollback(ctx)
	p.Logger.WaitForCopy(t.Name()+"_a", true)

	dumpA := dbTableDump(t, p.db, "_a")
	dumpB := dbTableDump(t, p.db, "_b")

	if assert.Len(t, dumpA, 2) {
		assert.EqualValues(t, 1, dumpA[0]["v"])
		assert.EqualValues(t, 2, dumpA[1]["v"])
	}
	if assert.Len(t, dumpB, 1) {
		assert.EqualValues(t, 3, dumpB[0]["v"])
	}

	// We should have had 3 connections. One for the lock, and one for each table.
	assert.EqualValues(t, 3, p.db.Stat().TotalConns())
}

// Test that the bad metric is dropped, and the rest of the batch succeeds.
func TestWrite_sequentialPermError(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 1}),
		newMetric(t, "_b", MSS{}, MSI{"v": 2}),
	}
	require.NoError(t, p.Write(metrics))

	metrics = []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": "a"}),
		newMetric(t, "_b", MSS{}, MSI{"v": 3}),
	}
	require.NoError(t, p.Write(metrics))

	dumpA := dbTableDump(t, p.db, "_a")
	dumpB := dbTableDump(t, p.db, "_b")
	assert.Len(t, dumpA, 1)
	assert.Len(t, dumpB, 2)

	haveError := false
	for _, l := range p.Logger.Logs() {
		if strings.Contains(l.String(), "write error") {
			haveError = true
			break
		}
	}
	assert.True(t, haveError, "write error not found in log")
}

// Test that the bad metric is dropped, and the rest of the batch succeeds.
func TestWrite_concurrentPermError(t *testing.T) {
	p := newPostgresqlTest(t)
	p.Connection += " pool_max_conns=2"
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))
	p.Logger.WaitForCopy(t.Name() + "_a", true)

	metrics = []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": "a"}),
		newMetric(t, "_b", MSS{}, MSI{"v": 2}),
	}
	require.NoError(t, p.Write(metrics))
	p.Logger.WaitFor(func(l Log) bool {
		return strings.Contains(l.String(), "write error")
	}, false)
	p.Logger.WaitForCopy(t.Name() + "_b", true)

	dumpA := dbTableDump(t, p.db, "_a")
	dumpB := dbTableDump(t, p.db, "_b")
	assert.Len(t, dumpA, 1)
	assert.Len(t, dumpB, 1)
}

// Verify that in sequential mode, errors are returned allowing telegraf agent to handle & retry
func TestWrite_sequentialTempError(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	// To avoid a race condition, we need to know when our goroutine has started listening to the log.
	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		// Wait for the CREATE TABLE, and then kill the connection.
		// The WaitFor callback holds a lock on the log. Meaning it will block logging of the next action. So we trigger
		// on CREATE TABLE so that there's a few statements to go before the COMMIT.
		p.Logger.WaitFor(func(log Log) bool {
			if strings.Contains(log.String(), "release wg") {
				wg.Done()
			}

			if !strings.Contains(log.String(), "CREATE TABLE") {
				return false
			}
			pid := log.args[1].(MSI)["pid"].(uint32)

			conf := p.db.Config().ConnConfig
			conf.Logger = nil
			c, err := pgx.ConnectConfig(context.Background(), conf)
			if !assert.NoError(t, err) {
				return true
			}
			_, err = c.Exec(context.Background(), "SELECT pg_terminate_backend($1)", pid)
			assert.NoError(t, err)
			return true
		}, false)
	}()

	p.Logger.Infof("release wg")
	wg.Wait()

	metrics := []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 1}),
	}
	require.Error(t, p.Write(metrics))
}

// Verify that when using concurrency, errors are not returned, but instead logged and automatically retried
func TestWrite_concurrentTempError(t *testing.T) {
	p := newPostgresqlTest(t)
	p.Connection += " pool_max_conns=2"
	require.NoError(t, p.Connect())

	// To avoid a race condition, we need to know when our goroutine has started listening to the log.
	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		// Wait for the CREATE TABLE, and then kill the connection.
		// The WaitFor callback holds a lock on the log. Meaning it will block logging of the next action. So we trigger
		// on CREATE TABLE so that there's a few statements to go before the COMMIT.
		p.Logger.WaitFor(func(log Log) bool {
			if strings.Contains(log.String(), "release wg") {
				wg.Done()
			}

			if !strings.Contains(log.String(), "CREATE TABLE") {
				return false
			}
			pid := log.args[1].(MSI)["pid"].(uint32)

			conf := p.db.Config().ConnConfig
			conf.Logger = nil
			c, err := pgx.ConnectConfig(context.Background(), conf)
			if !assert.NoError(t, err) {
				return true
			}
			_, err = c.Exec(context.Background(), "SELECT pg_terminate_backend($1)", pid)
			assert.NoError(t, err)
			return true
		}, false)
	}()
	p.Logger.Infof("release wg")
	wg.Wait()

	metrics := []telegraf.Metric{
		newMetric(t, "_a", MSS{}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))

	p.Logger.WaitForCopy(t.Name() + "_a", true)
	dumpA := dbTableDump(t, p.db, "_a")
	assert.Len(t, dumpA, 1)

	haveError := false
	for _, l := range p.Logger.Logs() {
		if strings.Contains(l.String(), "write error") {
			haveError = true
			break
		}
	}
	assert.True(t, haveError, "write error not found in log")
}

func TestWriteTagTable(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag":"foo"}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))

	dump := dbTableDump(t, p.db, "")
	require.Len(t, dump, 1)
	assert.EqualValues(t, 1, dump[0]["v"])

	dumpTags := dbTableDump(t, p.db, p.TagTableSuffix)
	require.Len(t, dumpTags, 1)
	assert.EqualValues(t, dump[0]["tag_id"], dumpTags[0]["tag_id"])
	assert.EqualValues(t, "foo", dumpTags[0]["tag"])
}

// Verify that when using TagsAsForeignKeys and a tag can't be written, that we still add the metrics.
func TestWrite_tagError(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag":"foo"}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))

	// It'll have the table cached, so won't know we dropped it, will try insert, and get error.
	_, err := p.db.Exec(ctx, "DROP TABLE \""+t.Name()+"_tag\"")
	require.NoError(t, err)

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag":"foo"}, MSI{"v": 2}),
	}
	require.NoError(t, p.Write(metrics))

	dump := dbTableDump(t, p.db, "")
	require.Len(t, dump, 2)
	assert.EqualValues(t, 1, dump[0]["v"])
	assert.EqualValues(t, 2, dump[1]["v"])
}

// Verify that when using TagsAsForeignKeys and ForeignTagConstraing and a tag can't be written, that we drop the metrics.
func TestWrite_tagError_foreignConstraint(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	p.ForignTagConstraint = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag":"foo"}, MSI{"v": 1}),
	}
	require.NoError(t, p.Write(metrics))

	// It'll have the table cached, so won't know we dropped it, will try insert, and get error.
	_, err := p.db.Exec(ctx, "DROP TABLE \""+t.Name()+"_tag\"")
	require.NoError(t, err)

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag":"foo"}, MSI{"v": 2}),
	}
	assert.NoError(t, p.Write(metrics))
	haveError := false
	for _, l := range p.Logger.Logs() {
		if strings.Contains(l.String(), "write error") {
			haveError = true
			break
		}
	}
	assert.True(t, haveError, "write error not found in log")

	dump := dbTableDump(t, p.db, "")
	require.Len(t, dump, 1)
	assert.EqualValues(t, 1, dump[0]["v"])
}
