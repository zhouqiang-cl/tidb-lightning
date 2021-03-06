// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package restore

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/cznic/mathutil"
	sstpb "github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb-lightning/lightning/common"
	"github.com/pingcap/tidb-lightning/lightning/config"
	"github.com/pingcap/tidb-lightning/lightning/kv"
	"github.com/pingcap/tidb-lightning/lightning/metric"
	"github.com/pingcap/tidb-lightning/lightning/mydump"
	verify "github.com/pingcap/tidb-lightning/lightning/verification"
	"github.com/pingcap/tidb-lightning/lightning/worker"

	"github.com/pingcap/errors"
	tidbcfg "github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/util/kvencoder"
)

const (
	FullLevelCompact = -1
	Level1Compact    = 1
)

const (
	defaultGCLifeTime = 100 * time.Hour
)

const (
	compactStateIdle int32 = iota
	compactStateDoing
)

var (
	requiredTiDBVersion = *semver.New("2.1.0")
	requiredPDVersion   = *semver.New("2.1.0")
	requiredTiKVVersion = *semver.New("2.1.0")
)

func init() {
	cfg := tidbcfg.GetGlobalConfig()
	cfg.Log.SlowThreshold = 3000

	kv.InitMembufCap(defReadBlockSize)
}

type saveCp struct {
	tableName string
	merger    TableCheckpointMerger
}

type errorSummary struct {
	status CheckpointStatus
	err    error
}
type errorSummaries struct {
	sync.Mutex
	summary map[string]errorSummary
}

func (es *errorSummaries) emitLog() {
	es.Lock()
	defer es.Unlock()
	if errorCount := len(es.summary); errorCount > 0 {
		var msg strings.Builder
		fmt.Fprintf(&msg, "Totally **%d** tables failed to be imported.\n", errorCount)
		for tableName, errorSummary := range es.summary {
			fmt.Fprintf(&msg, "- [%s] [%s] %s\n", tableName, errorSummary.status.MetricName(), errorSummary.err.Error())
		}
		common.AppLogger.Error(msg.String())
	}
}

func (es *errorSummaries) record(tableName string, err error, status CheckpointStatus) {
	es.Lock()
	defer es.Unlock()
	es.summary[tableName] = errorSummary{status: status, err: err}
}

type RestoreController struct {
	cfg             *config.Config
	dbMetas         []*mydump.MDDatabaseMeta
	dbInfos         map[string]*TidbDBInfo
	tableWorkers    *worker.Pool
	regionWorkers   *worker.Pool
	ioWorkers       *worker.Pool
	importer        *kv.Importer
	tidbMgr         *TiDBManager
	postProcessLock sync.Mutex // a simple way to ensure post-processing is not concurrent without using complicated goroutines
	alterTableLock  sync.Mutex
	compactState    int32

	errorSummaries errorSummaries

	checkpointsDB CheckpointsDB
	saveCpCh      chan saveCp
	checkpointsWg sync.WaitGroup
}

func NewRestoreController(ctx context.Context, dbMetas []*mydump.MDDatabaseMeta, cfg *config.Config) (*RestoreController, error) {
	importer, err := kv.NewImporter(ctx, cfg.TikvImporter.Addr, cfg.TiDB.PdAddr)
	if err != nil {
		return nil, errors.Trace(err)
	}

	cpdb, err := OpenCheckpointsDB(ctx, cfg)
	if err != nil {
		return nil, errors.Trace(err)
	}

	tidbMgr, err := NewTiDBManager(cfg.TiDB)
	if err != nil {
		return nil, errors.Trace(err)
	}

	rc := &RestoreController{
		cfg:           cfg,
		dbMetas:       dbMetas,
		tableWorkers:  worker.NewPool(ctx, cfg.App.TableConcurrency, "table"),
		regionWorkers: worker.NewPool(ctx, cfg.App.RegionConcurrency, "region"),
		ioWorkers:     worker.NewPool(ctx, cfg.App.IOConcurrency, "io"),
		importer:      importer,
		tidbMgr:       tidbMgr,

		errorSummaries: errorSummaries{
			summary: make(map[string]errorSummary),
		},

		checkpointsDB: cpdb,
		saveCpCh:      make(chan saveCp),
	}

	return rc, nil
}

func OpenCheckpointsDB(ctx context.Context, cfg *config.Config) (CheckpointsDB, error) {
	if !cfg.Checkpoint.Enable {
		return NewNullCheckpointsDB(), nil
	}

	switch cfg.Checkpoint.Driver {
	case "mysql":
		db, err := sql.Open("mysql", cfg.Checkpoint.DSN)
		if err != nil {
			return nil, errors.Trace(err)
		}
		cpdb, err := NewMySQLCheckpointsDB(ctx, db, cfg.Checkpoint.Schema)
		if err != nil {
			db.Close()
			return nil, errors.Trace(err)
		}
		return cpdb, nil

	case "file":
		return NewFileCheckpointsDB(cfg.Checkpoint.DSN), nil

	default:
		return nil, errors.Errorf("Unknown checkpoint driver %s", cfg.Checkpoint.Driver)
	}
}

func (rc *RestoreController) Wait() {
	rc.checkpointsWg.Wait()
}

func (rc *RestoreController) Close() {
	rc.importer.Close()
	rc.tidbMgr.Close()
}

func (rc *RestoreController) Run(ctx context.Context) error {
	timer := time.Now()
	opts := []func(context.Context) error{
		rc.checkRequirements,
		rc.restoreSchema,
		rc.restoreTables,
		rc.fullCompact,
		rc.switchToNormalMode,
		rc.cleanCheckpoints,
	}

	var err error
outside:
	for _, process := range opts {
		err = process(ctx)
		switch {
		case err == nil:
		case common.IsContextCanceledError(err):
			common.AppLogger.Infof("user terminated : %v", err)
			err = nil
			break outside
		default:
			common.AppLogger.Errorf("run cause error : %v", err)
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			break outside // ps : not continue
		}
	}

	common.AppLogger.Infof("the whole procedure takes %v", time.Since(timer))

	rc.errorSummaries.emitLog()

	return errors.Trace(err)
}

func (rc *RestoreController) restoreSchema(ctx context.Context) error {
	tidbMgr, err := NewTiDBManager(rc.cfg.TiDB)
	if err != nil {
		return errors.Trace(err)
	}
	defer tidbMgr.Close()

	if !rc.cfg.Mydumper.NoSchema {
		for _, dbMeta := range rc.dbMetas {
			timer := time.Now()
			common.AppLogger.Infof("restore table schema for `%s`", dbMeta.Name)
			tablesSchema := make(map[string]string)
			for _, tblMeta := range dbMeta.Tables {
				tablesSchema[tblMeta.Name] = tblMeta.GetSchema()
			}
			err = tidbMgr.InitSchema(ctx, dbMeta.Name, tablesSchema)
			if err != nil {
				return errors.Errorf("db schema failed to init : %v", err)
			}
			common.AppLogger.Infof("restore table schema for `%s` takes %v", dbMeta.Name, time.Since(timer))
		}
	}
	dbInfos, err := tidbMgr.LoadSchemaInfo(ctx, rc.dbMetas)
	if err != nil {
		return errors.Trace(err)
	}
	rc.dbInfos = dbInfos

	// Load new checkpoints
	err = rc.checkpointsDB.Initialize(ctx, dbInfos)
	if err != nil {
		return errors.Trace(err)
	}

	go rc.listenCheckpointUpdates(&rc.checkpointsWg)

	// Estimate the number of chunks for progress reporting
	rc.estimateChunkCountIntoMetrics()
	return nil
}

func (rc *RestoreController) estimateChunkCountIntoMetrics() {
	estimatedChunkCount := 0
	for _, dbMeta := range rc.dbMetas {
		for _, tableMeta := range dbMeta.Tables {
			estimatedChunkCount += len(tableMeta.DataFiles)
		}
	}
	metric.ChunkCounter.WithLabelValues(metric.ChunkStateEstimated).Add(float64(estimatedChunkCount))
}

func (rc *RestoreController) saveStatusCheckpoint(tableName string, engineID int, err error, statusIfSucceed CheckpointStatus) {
	merger := &StatusCheckpointMerger{Status: statusIfSucceed, EngineID: engineID}

	switch {
	case err == nil:
		break
	case !common.IsContextCanceledError(err):
		merger.SetInvalid()
		rc.errorSummaries.record(tableName, err, statusIfSucceed)
	default:
		return
	}

	metric.RecordTableCount(statusIfSucceed.MetricName(), err)
	rc.saveCpCh <- saveCp{tableName: tableName, merger: merger}
}

// listenCheckpointUpdates will combine several checkpoints together to reduce database load.
func (rc *RestoreController) listenCheckpointUpdates(wg *sync.WaitGroup) {
	var lock sync.Mutex
	coalesed := make(map[string]*TableCheckpointDiff)

	hasCheckpoint := make(chan struct{}, 1)

	go func() {
		for range hasCheckpoint {
			lock.Lock()
			cpd := coalesed
			coalesed = make(map[string]*TableCheckpointDiff)
			lock.Unlock()

			if len(cpd) > 0 {
				rc.checkpointsDB.Update(cpd)
			}
			wg.Done()
		}
	}()

	for scp := range rc.saveCpCh {
		lock.Lock()
		cpd, ok := coalesed[scp.tableName]
		if !ok {
			cpd = NewTableCheckpointDiff()
			coalesed[scp.tableName] = cpd
		}
		scp.merger.MergeInto(cpd)

		if len(hasCheckpoint) == 0 {
			wg.Add(1)
			hasCheckpoint <- struct{}{}
		}

		lock.Unlock()

		// gofail: var FailIfImportedChunk struct{}
		// if _, ok := scp.merger.(*ChunkCheckpointMerger); ok {
		// 	wg.Wait()
		// 	panic("forcing failure due to FailIfImportedChunk")
		// }
		// continue

		// gofail: var FailIfStatusBecomes int
		// if merger, ok := scp.merger.(*StatusCheckpointMerger); ok && merger.EngineID >= 0 && int(merger.Status) == FailIfStatusBecomes {
		// 	wg.Wait()
		// 	panic("forcing failure due to FailIfStatusBecomes")
		// }
		// continue
	}
}

func (rc *RestoreController) runPeriodicActions(ctx context.Context, stop <-chan struct{}) {
	switchModeTicker := time.NewTicker(rc.cfg.Cron.SwitchMode.Duration)
	logProgressTicker := time.NewTicker(rc.cfg.Cron.LogProgress.Duration)
	defer func() {
		switchModeTicker.Stop()
		logProgressTicker.Stop()
	}()

	rc.switchToImportMode(ctx)

	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			common.AppLogger.Warnf("Stopping periodic actions due to %v", ctx.Err())
			return
		case <-stop:
			common.AppLogger.Info("Everything imported, stopping periodic actions")
			return

		case <-switchModeTicker.C:
			// periodically switch to import mode, as requested by TiKV 3.0
			rc.switchToImportMode(ctx)

		case <-logProgressTicker.C:
			// log the current progress periodically, so OPS will know that we're still working
			nanoseconds := float64(time.Since(start).Nanoseconds())
			estimated := metric.ReadCounter(metric.ChunkCounter.WithLabelValues(metric.ChunkStateEstimated))
			finished := metric.ReadCounter(metric.ChunkCounter.WithLabelValues(metric.ChunkStateFinished))
			totalTables := metric.ReadCounter(metric.TableCounter.WithLabelValues(metric.TableStatePending, metric.TableResultSuccess))
			completedTables := metric.ReadCounter(metric.TableCounter.WithLabelValues(metric.TableStateCompleted, metric.TableResultSuccess))
			bytesRead := metric.ReadHistogramSum(metric.BlockReadBytesHistogram)

			var remaining string
			if finished >= estimated {
				remaining = ", post-processing"
			} else if finished > 0 {
				remainNanoseconds := (estimated/finished - 1) * nanoseconds
				remaining = fmt.Sprintf(", remaining %s", time.Duration(remainNanoseconds).Round(time.Second))
			}

			// Note: a speed of 28 MiB/s roughly corresponds to 100 GiB/hour.
			common.AppLogger.Infof(
				"progress: %.0f/%.0f chunks (%.1f%%), %.0f/%.0f tables (%.1f%%), speed %.2f MiB/s%s",
				finished, estimated, finished/estimated*100,
				completedTables, totalTables, completedTables/totalTables*100,
				bytesRead/(1048576e-9*nanoseconds),
				remaining,
			)
		}
	}
}

func (rc *RestoreController) restoreTables(ctx context.Context) error {
	timer := time.Now()
	var wg sync.WaitGroup

	var restoreErr common.OnceError

	stopPeriodicActions := make(chan struct{}, 1)
	go rc.runPeriodicActions(ctx, stopPeriodicActions)

	for _, dbMeta := range rc.dbMetas {
		dbInfo, ok := rc.dbInfos[dbMeta.Name]
		if !ok {
			common.AppLogger.Errorf("database %s not found in rc.dbInfos", dbMeta.Name)
			continue
		}
		for _, tableMeta := range dbMeta.Tables {
			tableInfo, ok := dbInfo.Tables[tableMeta.Name]
			if !ok {
				return errors.Errorf("table info %s not found", tableMeta.Name)
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			tableName := common.UniqueTable(dbInfo.Name, tableInfo.Name)
			cp, err := rc.checkpointsDB.Get(ctx, tableName)
			if cp.Status <= CheckpointStatusMaxInvalid {
				return errors.Errorf("Checkpoint for %s has invalid status: %d", tableName, cp.Status)
			}
			if err != nil {
				return errors.Trace(err)
			}
			tr, err := NewTableRestore(tableName, tableMeta, dbInfo, tableInfo, cp)
			if err != nil {
				return errors.Trace(err)
			}

			wg.Add(1)
			go func(t *TableRestore, cp *TableCheckpoint) {
				defer wg.Done()
				err := t.restoreTable(ctx, rc, cp)
				metric.RecordTableCount("completed", err)
				restoreErr.Set(t.tableName, err)
			}(tr, cp)
		}
	}

	wg.Wait()
	stopPeriodicActions <- struct{}{}
	common.AppLogger.Infof("restore all tables data takes %v", time.Since(timer))

	return errors.Trace(restoreErr.Get())
}

func (t *TableRestore) restoreTable(
	ctx context.Context,
	rc *RestoreController,
	cp *TableCheckpoint,
) error {
	// 1. Load the table info.

	// no need to do anything if the chunks are already populated
	if len(cp.Engines) > 0 {
		common.AppLogger.Infof("[%s] reusing %d engines and %d chunks from checkpoint", t.tableName, len(cp.Engines), cp.CountChunks())
	} else if cp.Status < CheckpointStatusAllWritten {
		if err := t.populateChunks(rc.cfg, cp); err != nil {
			return errors.Trace(err)
		}
		if err := rc.checkpointsDB.InsertEngineCheckpoints(ctx, t.tableName, cp.Engines); err != nil {
			return errors.Trace(err)
		}

		// rebase the allocator so it exceeds the number of rows.
		cp.AllocBase = mathutil.MaxInt64(cp.AllocBase, t.tableInfo.core.AutoIncID)
		for _, engine := range cp.Engines {
			for _, chunk := range engine.Chunks {
				cp.AllocBase = mathutil.MaxInt64(cp.AllocBase, chunk.Chunk.RowIDMax)
			}
		}
		t.alloc.Rebase(t.tableInfo.ID, cp.AllocBase, false)
		rc.saveCpCh <- saveCp{
			tableName: t.tableName,
			merger: &RebaseCheckpointMerger{
				AllocBase: cp.AllocBase,
			},
		}
	}

	// 2. Restore engines (if still needed)

	if cp.Status < CheckpointStatusImported {
		timer := time.Now()

		var wg sync.WaitGroup
		var engineErr common.OnceError

		for engineID, engine := range cp.Engines {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if engineErr.Get() != nil {
				break
			}

			wg.Add(1)

			// Note: We still need tableWorkers to control the concurrency of tables.
			// In the future, we will investigate more about
			// the difference between restoring tables concurrently and restoring tables one by one.
			restoreWorker := rc.tableWorkers.Apply()

			go func(w *worker.Worker, eid int, ecp *EngineCheckpoint) {
				defer wg.Done()
				tag := fmt.Sprintf("%s:%d", t.tableName, eid)

				closedEngine, err := t.restoreEngine(ctx, rc, eid, ecp)
				rc.tableWorkers.Recycle(w)
				if err != nil {
					engineErr.Set(tag, err)
					return
				}
				if err := t.importEngine(ctx, closedEngine, rc, eid, ecp); err != nil {
					engineErr.Set(tag, err)
				}
			}(restoreWorker, engineID, engine)
		}

		wg.Wait()

		common.AppLogger.Infof("[%s] import whole table takes %v", t.tableName, time.Since(timer))
		err := engineErr.Get()
		rc.saveStatusCheckpoint(t.tableName, -1, err, CheckpointStatusImported)
		if err != nil {
			return errors.Trace(err)
		}
	}

	// 3. Post-process

	return errors.Trace(t.postProcess(ctx, rc, cp))
}

func (t *TableRestore) restoreEngine(
	ctx context.Context,
	rc *RestoreController,
	engineID int,
	cp *EngineCheckpoint,
) (*kv.ClosedEngine, error) {
	if cp.Status >= CheckpointStatusClosed {
		closedEngine, err := rc.importer.UnsafeCloseEngine(ctx, t.tableName, engineID)
		return closedEngine, errors.Trace(err)
	}

	timer := time.Now()

	engine, err := rc.importer.OpenEngine(ctx, t.tableName, engineID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	var wg sync.WaitGroup
	var chunkErr common.OnceError

	// Restore table data
	for chunkIndex, chunk := range cp.Chunks {
		if chunk.Chunk.Offset >= chunk.Chunk.EndOffset {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if chunkErr.Get() != nil {
			break
		}

		// Flows :
		// 	1. read mydump file
		// 	2. sql -> kvs
		// 	3. load kvs data (into kv deliver server)
		// 	4. flush kvs data (into tikv node)

		cr, err := newChunkRestore(chunkIndex, chunk, rc.cfg.Mydumper.ReadBlockSize, rc.ioWorkers)
		if err != nil {
			return nil, errors.Trace(err)
		}
		metric.ChunkCounter.WithLabelValues(metric.ChunkStatePending).Inc()

		restoreWorker := rc.regionWorkers.Apply()
		wg.Add(1)
		go func(w *worker.Worker, cr *chunkRestore) {
			// Restore a chunk.
			defer func() {
				cr.close()
				wg.Done()
				rc.regionWorkers.Recycle(w)
			}()
			metric.ChunkCounter.WithLabelValues(metric.ChunkStateRunning).Inc()
			err := cr.restore(ctx, t, engineID, engine, rc)
			if err == nil {
				metric.ChunkCounter.WithLabelValues(metric.ChunkStateFinished).Inc()
				return
			}
			metric.ChunkCounter.WithLabelValues(metric.ChunkStateFailed).Inc()
			tag := fmt.Sprintf("%s:%d] [%s", t.tableName, engineID, &cr.chunk.Key)
			chunkErr.Set(tag, err)
		}(restoreWorker, cr)
	}

	wg.Wait()
	dur := time.Since(timer)

	// Report some statistics into the log for debugging.
	totalKVSize := uint64(0)
	totalSQLSize := int64(0)
	for _, chunk := range cp.Chunks {
		totalKVSize += chunk.Checksum.SumSize()
		totalSQLSize += chunk.Chunk.EndOffset
	}

	common.AppLogger.Infof("[%s:%d] encode kv data and write takes %v (read %d, written %d)", t.tableName, engineID, dur, totalSQLSize, totalKVSize)
	err = chunkErr.Get()
	rc.saveStatusCheckpoint(t.tableName, engineID, err, CheckpointStatusAllWritten)
	if err != nil {
		return nil, errors.Trace(err)
	}

	closedEngine, err := engine.Close(ctx)
	rc.saveStatusCheckpoint(t.tableName, engineID, err, CheckpointStatusClosed)
	if err != nil {
		common.AppLogger.Errorf("[kv-deliver] flush stage with error (step = close) : %s", errors.ErrorStack(err))
		return nil, errors.Trace(err)
	}
	return closedEngine, nil
}

func (t *TableRestore) importEngine(
	ctx context.Context,
	closedEngine *kv.ClosedEngine,
	rc *RestoreController,
	engineID int,
	cp *EngineCheckpoint,
) error {
	if cp.Status >= CheckpointStatusImported {
		return nil
	}

	// 1. close engine, then calling import
	// FIXME: flush is an asynchronous operation, what if flush failed?

	// the lock ensures the import() step will not be concurrent.
	rc.postProcessLock.Lock()
	err := t.importKV(ctx, closedEngine)
	// gofail: var SlowDownImport struct{}
	rc.postProcessLock.Unlock()
	rc.saveStatusCheckpoint(t.tableName, engineID, err, CheckpointStatusImported)
	if err != nil {
		return errors.Trace(err)
	}

	// 2. perform a level-1 compact if idling.
	if atomic.CompareAndSwapInt32(&rc.compactState, compactStateIdle, compactStateDoing) {
		go func() {
			err := rc.doCompact(ctx, Level1Compact)
			if err != nil {
				// log it and continue
				common.AppLogger.Warnf("compact %d failed %v", Level1Compact, err)
			}
			atomic.StoreInt32(&rc.compactState, compactStateIdle)
		}()
	}

	return nil
}

func (t *TableRestore) postProcess(ctx context.Context, rc *RestoreController, cp *TableCheckpoint) error {
	setSessionConcurrencyVars(ctx, rc.tidbMgr.db, rc.cfg.TiDB)

	// 3. alter table set auto_increment
	if cp.Status < CheckpointStatusAlteredAutoInc {
		rc.alterTableLock.Lock()
		err := t.restoreTableMeta(ctx, rc.tidbMgr.db)
		rc.alterTableLock.Unlock()
		rc.saveStatusCheckpoint(t.tableName, -1, err, CheckpointStatusAlteredAutoInc)
		if err != nil {
			common.AppLogger.Errorf(
				"[%[1]s] failed to AUTO TABLE %[1]s SET AUTO_INCREMENT=%[2]d : %[3]v",
				t.tableName, t.alloc.Base()+1, err.Error(),
			)
			return errors.Trace(err)
		}
	}

	// 4. do table checksum
	if cp.Status < CheckpointStatusChecksummed {
		if !rc.cfg.PostRestore.Checksum {
			common.AppLogger.Infof("[%s] Skip checksum.", t.tableName)
			rc.saveStatusCheckpoint(t.tableName, -1, nil, CheckpointStatusChecksumSkipped)
		} else {
			err := t.compareChecksum(ctx, rc.tidbMgr.db, cp)
			rc.saveStatusCheckpoint(t.tableName, -1, err, CheckpointStatusChecksummed)
			if err != nil {
				common.AppLogger.Errorf("[%s] checksum failed: %v", t.tableName, err.Error())
				return errors.Trace(err)
			}
		}
	}

	// 5. do table analyze
	if cp.Status < CheckpointStatusAnalyzed {
		if !rc.cfg.PostRestore.Analyze {
			common.AppLogger.Infof("[%s] Skip analyze.", t.tableName)
			rc.saveStatusCheckpoint(t.tableName, -1, nil, CheckpointStatusAnalyzeSkipped)
		} else {
			err := t.analyzeTable(ctx, rc.tidbMgr.db)
			rc.saveStatusCheckpoint(t.tableName, -1, err, CheckpointStatusAnalyzed)
			if err != nil {
				common.AppLogger.Errorf("[%s] analyze failed: %v", t.tableName, err.Error())
				return errors.Trace(err)
			}
		}
	}

	return nil
}

// do full compaction for the whole data.
func (rc *RestoreController) fullCompact(ctx context.Context) error {
	if !rc.cfg.PostRestore.Compact {
		common.AppLogger.Info("Skip full compaction.")
		return nil
	}

	// wait until any existing level-1 compact to complete first.
	common.AppLogger.Info("Wait for existing level 1 compaction to finish")
	start := time.Now()
	for !atomic.CompareAndSwapInt32(&rc.compactState, compactStateIdle, compactStateDoing) {
		time.Sleep(100 * time.Millisecond)
	}
	common.AppLogger.Infof("Wait for existing level 1 compaction to finish takes %v", time.Since(start))

	return errors.Trace(rc.doCompact(ctx, FullLevelCompact))
}

func (rc *RestoreController) doCompact(ctx context.Context, level int32) error {
	return errors.Trace(rc.importer.Compact(ctx, level))
}

func (rc *RestoreController) switchToImportMode(ctx context.Context) {
	rc.switchTiKVMode(ctx, sstpb.SwitchMode_Import)
}

func (rc *RestoreController) switchToNormalMode(ctx context.Context) error {
	rc.switchTiKVMode(ctx, sstpb.SwitchMode_Normal)
	return nil
}

func (rc *RestoreController) switchTiKVMode(ctx context.Context, mode sstpb.SwitchMode) {
	if err := rc.importer.SwitchMode(ctx, mode); err != nil {
		common.AppLogger.Warnf("cannot switch to %s mode: %v", mode.String(), err)
	}
}

func (rc *RestoreController) checkRequirements(_ context.Context) error {
	// skip requirement check if explicitly turned off
	if !rc.cfg.App.CheckRequirements {
		return nil
	}

	client := &http.Client{}
	if err := rc.checkTiDBVersion(client); err != nil {
		return errors.Trace(err)
	}
	if err := rc.checkPDVersion(client); err != nil {
		return errors.Trace(err)
	}
	if err := rc.checkTiKVVersion(client); err != nil {
		return errors.Trace(err)
	}

	return nil
}

func extractTiDBVersion(version string) (*semver.Version, error) {
	// version format: "5.7.10-TiDB-v2.1.0-rc.1-7-g38c939f"
	//                               ^~~~~~~~~^ we only want this part
	// version format: "5.7.10-TiDB-v2.0.4-1-g06a0bf5"
	//                               ^~~~^
	// version format: "5.7.10-TiDB-v2.0.7"
	//                               ^~~~^
	// The version is generated by `git describe --tags` on the TiDB repository.
	versions := strings.Split(version, "-")
	end := len(versions)
	switch end {
	case 3, 4:
	case 5, 6:
		end -= 2
	default:
		return nil, errors.Errorf("not a valid TiDB version: %s", version)
	}
	rawVersion := strings.Join(versions[2:end], "-")
	rawVersion = strings.TrimPrefix(rawVersion, "v")
	return semver.NewVersion(rawVersion)
}

func (rc *RestoreController) checkTiDBVersion(client *http.Client) error {
	url := fmt.Sprintf("http://%s:%d/status", rc.cfg.TiDB.Host, rc.cfg.TiDB.StatusPort)
	var status struct{ Version string }
	err := common.GetJSON(client, url, &status)
	if err != nil {
		return errors.Trace(err)
	}

	version, err := extractTiDBVersion(status.Version)
	if err != nil {
		return errors.Trace(err)
	}
	return checkVersion("TiDB", requiredTiDBVersion, *version)
}

func (rc *RestoreController) checkPDVersion(client *http.Client) error {
	url := fmt.Sprintf("http://%s/pd/api/v1/config/cluster-version", rc.cfg.TiDB.PdAddr)
	var rawVersion string
	err := common.GetJSON(client, url, &rawVersion)
	if err != nil {
		return errors.Trace(err)
	}

	version, err := semver.NewVersion(rawVersion)
	if err != nil {
		return errors.Trace(err)
	}

	return checkVersion("PD", requiredPDVersion, *version)
}

func (rc *RestoreController) checkTiKVVersion(client *http.Client) error {
	url := fmt.Sprintf("http://%s/pd/api/v1/stores", rc.cfg.TiDB.PdAddr)

	var stores struct {
		Stores []struct {
			Store struct {
				Address string
				Version string
			}
		}
	}
	err := common.GetJSON(client, url, &stores)
	if err != nil {
		return errors.Trace(err)
	}

	for _, store := range stores.Stores {
		version, err := semver.NewVersion(store.Store.Version)
		if err != nil {
			return errors.Annotate(err, store.Store.Address)
		}
		component := fmt.Sprintf("TiKV (at %s)", store.Store.Address)
		err = checkVersion(component, requiredTiKVVersion, *version)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func checkVersion(component string, expected, actual semver.Version) error {
	if actual.Compare(expected) >= 0 {
		return nil
	}
	return errors.Errorf(
		"%s version too old, expected '>=%s', found '%s'",
		component,
		expected,
		actual,
	)
}

func (rc *RestoreController) cleanCheckpoints(ctx context.Context) error {
	if !rc.cfg.Checkpoint.Enable || rc.cfg.Checkpoint.KeepAfterSuccess {
		common.AppLogger.Info("Skip clean checkpoints.")

		return nil
	}
	timer := time.Now()
	err := rc.checkpointsDB.RemoveCheckpoint(ctx, "all")
	common.AppLogger.Infof("clean checkpoints takes %v", time.Since(timer))
	return errors.Trace(err)
}

type chunkRestore struct {
	parser *mydump.ChunkParser
	index  int
	chunk  *ChunkCheckpoint
}

func newChunkRestore(index int, chunk *ChunkCheckpoint, blockBufSize int64, ioWorkers *worker.Pool) (*chunkRestore, error) {
	reader, err := os.Open(chunk.Key.Path)
	if err != nil {
		return nil, errors.Trace(err)
	}
	parser := mydump.NewChunkParser(reader, blockBufSize, ioWorkers)

	reader.Seek(chunk.Chunk.Offset, io.SeekStart)
	parser.SetPos(chunk.Chunk.Offset, chunk.Chunk.PrevRowIDMax)

	return &chunkRestore{
		parser: parser,
		index:  index,
		chunk:  chunk,
	}, nil
}

func (cr *chunkRestore) close() {
	cr.parser.Reader().(*os.File).Close()
}

type TableRestore struct {
	// The unique table name in the form "`db`.`tbl`".
	tableName string
	dbInfo    *TidbDBInfo
	tableInfo *TidbTableInfo
	tableMeta *mydump.MDTableMeta
	encoder   kvenc.KvEncoder
	alloc     autoid.Allocator
}

func NewTableRestore(
	tableName string,
	tableMeta *mydump.MDTableMeta,
	dbInfo *TidbDBInfo,
	tableInfo *TidbTableInfo,
	cp *TableCheckpoint,
) (*TableRestore, error) {
	idAlloc := kv.NewPanickingAllocator(cp.AllocBase)
	encoder, err := kvenc.New(dbInfo.Name, idAlloc)
	if err != nil {
		return nil, errors.Annotatef(err, "failed to kvenc.New %s", tableName)
	}
	// create table in encoder.
	err = encoder.ExecDDLSQL(tableInfo.CreateTableStmt)
	if err != nil {
		return nil, errors.Annotatef(err, "failed to ExecDDLSQL %s", tableName)
	}

	return &TableRestore{
		tableName: tableName,
		dbInfo:    dbInfo,
		tableInfo: tableInfo,
		tableMeta: tableMeta,
		encoder:   encoder,
		alloc:     idAlloc,
	}, nil
}

func (tr *TableRestore) Close() {
	tr.encoder.Close()
	common.AppLogger.Infof("[%s] restore done", tr.tableName)
}

var tidbRowIDColumnRegex = regexp.MustCompile(fmt.Sprintf("`%[1]s`|(?i:\\b%[1]s\\b)", model.ExtraHandleName))

func (t *TableRestore) populateChunks(cfg *config.Config, cp *TableCheckpoint) error {
	common.AppLogger.Infof("[%s] load chunks", t.tableName)
	timer := time.Now()

	chunks, err := mydump.MakeTableRegions(t.tableMeta, t.tableInfo.Columns, cfg.Mydumper.BatchSize, cfg.Mydumper.BatchImportRatio, cfg.App.TableConcurrency)
	if err != nil {
		return errors.Trace(err)
	}

	for _, chunk := range chunks {
		for chunk.EngineID >= len(cp.Engines) {
			cp.Engines = append(cp.Engines, &EngineCheckpoint{Status: CheckpointStatusLoaded})
		}
		cp.Engines[chunk.EngineID].Chunks = append(cp.Engines[chunk.EngineID].Chunks, &ChunkCheckpoint{
			Key: ChunkCheckpointKey{
				Path:   chunk.File,
				Offset: chunk.Chunk.Offset,
			},
			Columns: nil,
			Chunk:   chunk.Chunk,
		})
	}

	common.AppLogger.Infof("[%s] load %d engines and %d chunks takes %v", t.tableName, len(cp.Engines), len(chunks), time.Since(timer))
	return nil
}

func (t *TableRestore) initializeColumns(columns []byte, ccp *ChunkCheckpoint) {
	shouldIncludeRowID := !t.tableInfo.core.PKIsHandle && !tidbRowIDColumnRegex.Match(columns)
	if shouldIncludeRowID {
		// we need to inject the _tidb_rowid column
		if len(columns) != 0 {
			// column listing already exists, just append the new column.
			columns = append(columns[:len(columns)-1], (",`" + model.ExtraHandleName.String() + "`)")...)
		} else {
			// we need to recreate the columns
			var buf bytes.Buffer
			buf.WriteString("(`")
			for _, columnInfo := range t.tableInfo.core.Columns {
				buf.WriteString(columnInfo.Name.String())
				buf.WriteString("`,`")
			}
			buf.WriteString(model.ExtraHandleName.String())
			buf.WriteString("`)")
			columns = buf.Bytes()
		}
	} else if columns == nil {
		columns = []byte{}
	}
	ccp.Columns = columns
	ccp.ShouldIncludeRowID = shouldIncludeRowID
}

func (tr *TableRestore) restoreTableMeta(ctx context.Context, db *sql.DB) error {
	timer := time.Now()

	err := AlterAutoIncrement(ctx, db, tr.tableMeta.DB, tr.tableMeta.Name, tr.alloc.Base()+1)
	if err != nil {
		return errors.Trace(err)
	}
	common.AppLogger.Infof("[%s] alter table set auto_id takes %v", common.UniqueTable(tr.tableMeta.DB, tr.tableMeta.Name), time.Since(timer))
	return nil
}

func (tr *TableRestore) importKV(ctx context.Context, closedEngine *kv.ClosedEngine) error {
	common.AppLogger.Infof("[%s] flush kv deliver ...", tr.tableName)

	start := time.Now()

	err := closedEngine.Import(ctx)
	if err != nil {
		if !common.IsContextCanceledError(err) {
			common.AppLogger.Errorf("[%s] failed to flush kvs : %s", tr.tableName, err.Error())
		}
		return errors.Trace(err)
	}
	closedEngine.Cleanup(ctx)

	dur := time.Since(start)
	metric.ImportSecondsHistogram.Observe(dur.Seconds())
	common.AppLogger.Infof("[%s] kv deliver all flushed, takes %v", tr.tableName, dur)

	return nil
}

// do checksum for each table.
func (tr *TableRestore) compareChecksum(ctx context.Context, db *sql.DB, cp *TableCheckpoint) error {
	var localChecksum verify.KVChecksum
	for _, engine := range cp.Engines {
		for _, chunk := range engine.Chunks {
			localChecksum.Add(&chunk.Checksum)
		}
	}

	start := time.Now()
	remoteChecksum, err := DoChecksum(ctx, db, tr.tableName)
	dur := time.Since(start)
	metric.ChecksumSecondsHistogram.Observe(dur.Seconds())
	if err != nil {
		return errors.Trace(err)
	}

	if remoteChecksum.Checksum != localChecksum.Sum() ||
		remoteChecksum.TotalKVs != localChecksum.SumKVS() ||
		remoteChecksum.TotalBytes != localChecksum.SumSize() {
		return errors.Errorf("checksum mismatched remote vs local => (checksum: %d vs %d) (total_kvs: %d vs %d) (total_bytes:%d vs %d)",
			remoteChecksum.Checksum, localChecksum.Sum(),
			remoteChecksum.TotalKVs, localChecksum.SumKVS(),
			remoteChecksum.TotalBytes, localChecksum.SumSize(),
		)
	}

	common.AppLogger.Infof("[%s] checksum pass, %+v takes %v", tr.tableName, localChecksum, dur)
	return nil
}

func (tr *TableRestore) analyzeTable(ctx context.Context, db *sql.DB) error {
	timer := time.Now()
	common.AppLogger.Infof("[%s] analyze", tr.tableName)
	query := fmt.Sprintf("ANALYZE TABLE %s", tr.tableName)
	err := common.ExecWithRetry(ctx, db, query, query)
	if err != nil {
		return errors.Trace(err)
	}
	common.AppLogger.Infof("[%s] analyze takes %v", tr.tableName, time.Since(timer))
	return nil
}

// RemoteChecksum represents a checksum result got from tidb.
type RemoteChecksum struct {
	Schema     string
	Table      string
	Checksum   uint64
	TotalKVs   uint64
	TotalBytes uint64
}

func (c *RemoteChecksum) String() string {
	return fmt.Sprintf("[%s] remote_checksum=%d, total_kvs=%d, total_bytes=%d", common.UniqueTable(c.Schema, c.Table), c.Checksum, c.TotalKVs, c.TotalBytes)
}

func setSessionConcurrencyVars(ctx context.Context, db *sql.DB, dsn config.DBStore) {
	err := common.ExecWithRetry(ctx, db, "(set session concurrency variables)", `SET
		SESSION tidb_build_stats_concurrency = ?,
		SESSION tidb_distsql_scan_concurrency = ?,
		SESSION tidb_index_serial_scan_concurrency = ?,
		SESSION tidb_checksum_table_concurrency = ?;
	`, dsn.BuildStatsConcurrency, dsn.DistSQLScanConcurrency, dsn.IndexSerialScanConcurrency, dsn.ChecksumTableConcurrency)
	if err != nil {
		common.AppLogger.Warnf("failed to set session concurrency variables: %s", err.Error())
	}
}

// DoChecksum do checksum for tables.
// table should be in <db>.<table>, format.  e.g. foo.bar
func DoChecksum(ctx context.Context, db *sql.DB, table string) (*RemoteChecksum, error) {
	timer := time.Now()

	ori, err := increaseGCLifeTime(ctx, db)
	if err != nil {
		return nil, errors.Trace(err)
	}
	// set it back finally
	defer func() {
		err = UpdateGCLifeTime(ctx, db, ori)
		if err != nil && !common.IsContextCanceledError(err) {
			common.AppLogger.Errorf("[%s] update tikv_gc_life_time error %v", table, errors.ErrorStack(err))
		}
	}()

	// ADMIN CHECKSUM TABLE <table>,<table>  example.
	// 	mysql> admin checksum table test.t;
	// +---------+------------+---------------------+-----------+-------------+
	// | Db_name | Table_name | Checksum_crc64_xor  | Total_kvs | Total_bytes |
	// +---------+------------+---------------------+-----------+-------------+
	// | test    | t          | 8520875019404689597 |   7296873 |   357601387 |
	// +---------+------------+---------------------+-----------+-------------+

	cs := RemoteChecksum{}
	common.AppLogger.Infof("[%s] doing remote checksum", table)
	query := fmt.Sprintf("ADMIN CHECKSUM TABLE %s", table)
	err = common.QueryRowWithRetry(ctx, db, query, &cs.Schema, &cs.Table, &cs.Checksum, &cs.TotalKVs, &cs.TotalBytes)
	if err != nil {
		return nil, errors.Trace(err)
	}
	common.AppLogger.Infof("[%s] do checksum takes %v", table, time.Since(timer))

	return &cs, nil
}

func increaseGCLifeTime(ctx context.Context, db *sql.DB) (oriGCLifeTime string, err error) {
	// checksum command usually takes a long time to execute,
	// so here need to increase the gcLifeTime for single transaction.
	oriGCLifeTime, err = ObtainGCLifeTime(ctx, db)
	if err != nil {
		return "", errors.Trace(err)
	}

	var increaseGCLifeTime bool
	if oriGCLifeTime != "" {
		ori, err := time.ParseDuration(oriGCLifeTime)
		if err != nil {
			return "", errors.Trace(err)
		}
		if ori < defaultGCLifeTime {
			increaseGCLifeTime = true
		}
	} else {
		increaseGCLifeTime = true
	}

	if increaseGCLifeTime {
		err = UpdateGCLifeTime(ctx, db, defaultGCLifeTime.String())
		if err != nil {
			return "", errors.Trace(err)
		}
	}

	return oriGCLifeTime, nil
}

////////////////////////////////////////////////////////////////

const (
	maxKVQueueSize  = 128
	maxDeliverBytes = 31 << 20 // 31 MB. hardcoded by importer, so do we
)

func splitIntoDeliveryStreams(totalKVs []kvenc.KvPair, splitSize int) [][]kvenc.KvPair {
	res := make([][]kvenc.KvPair, 0, 1)
	i := 0
	cumSize := 0

	for j, pair := range totalKVs {
		size := len(pair.Key) + len(pair.Val)
		if i < j && cumSize+size > splitSize {
			res = append(res, totalKVs[i:j])
			i = j
			cumSize = 0
		}
		cumSize += size
	}

	return append(res, totalKVs[i:])
}

func (cr *chunkRestore) restore(
	ctx context.Context,
	t *TableRestore,
	engineID int,
	engine *kv.OpenedEngine,
	rc *RestoreController,
) error {
	// Create the encoder.
	kvEncoder, err := kv.NewTableKVEncoder(
		t.dbInfo.Name,
		t.tableInfo.Name,
		t.tableInfo.ID,
		rc.cfg.TiDB.SQLMode,
		t.alloc,
	)
	if err != nil {
		return errors.Trace(err)
	}
	defer func() {
		closeErr := kvEncoder.Close()
		kvEncoder = nil
		if closeErr != nil {
			common.AppLogger.Errorf("restore chunk task err %v", errors.ErrorStack(closeErr))
		}
	}()

	timer := time.Now()
	readTotalDur := time.Duration(0)
	encodeTotalDur := time.Duration(0)
	deliverTotalDur := time.Duration(0)

	var block struct {
		cond            *sync.Cond
		encodeCompleted bool
		totalKVs        []kvenc.KvPair
		localChecksum   verify.KVChecksum
		chunkOffset     int64
		chunkRowID      int64
	}
	block.cond = sync.NewCond(new(sync.Mutex))
	deliverCompleteCh := make(chan error, 1)

	go func() {
		for {
			block.cond.L.Lock()
			for !block.encodeCompleted && len(block.totalKVs) == 0 {
				block.cond.Wait()
			}
			b := block
			block.totalKVs = nil
			block.localChecksum = verify.MakeKVChecksum(0, 0, 0)
			block.cond.L.Unlock()

			if b.encodeCompleted && len(b.totalKVs) == 0 {
				deliverCompleteCh <- nil
				return
			}

			// kv -> deliver ( -> tikv )
			start := time.Now()
			stream, err := engine.NewWriteStream(ctx)
			if err != nil {
				deliverCompleteCh <- errors.Trace(err)
				return
			}

			for _, kvs := range splitIntoDeliveryStreams(b.totalKVs, maxDeliverBytes) {
				if e := stream.Put(kvs); e != nil {
					if err != nil {
						common.AppLogger.Warnf("failed to put write stream: %s", e.Error())
					} else {
						err = e
					}
				}
			}
			b.totalKVs = nil

			block.cond.Signal()
			if e := stream.Close(); e != nil {
				if err != nil {
					common.AppLogger.Warnf("[%s:%d] failed to close write stream: %s", t.tableName, engineID, e.Error())
				} else {
					err = e
				}
			}
			deliverDur := time.Since(start)
			deliverTotalDur += deliverDur
			metric.BlockDeliverSecondsHistogram.Observe(deliverDur.Seconds())
			metric.BlockDeliverBytesHistogram.Observe(float64(b.localChecksum.SumSize()))

			if err != nil {
				if !common.IsContextCanceledError(err) {
					common.AppLogger.Errorf("[%s:%d] kv deliver failed = %v", t.tableName, engineID, err)
				}
				// TODO : retry ~
				deliverCompleteCh <- errors.Trace(err)
				return
			}

			// Update the table, and save a checkpoint.
			// (the write to the importer is effective immediately, thus update these here)
			cr.chunk.Checksum.Add(&b.localChecksum)
			cr.chunk.Chunk.Offset = b.chunkOffset
			cr.chunk.Chunk.PrevRowIDMax = b.chunkRowID
			rc.saveCpCh <- saveCp{
				tableName: t.tableName,
				merger: &RebaseCheckpointMerger{
					AllocBase: t.alloc.Base() + 1,
				},
			}
			rc.saveCpCh <- saveCp{
				tableName: t.tableName,
				merger: &ChunkCheckpointMerger{
					EngineID: engineID,
					Key:      cr.chunk.Key,
					Checksum: cr.chunk.Checksum,
					Pos:      cr.chunk.Chunk.Offset,
					RowID:    cr.chunk.Chunk.PrevRowIDMax,
				},
			}
		}
	}()

	var buffer bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		endOffset := mathutil.MinInt64(cr.chunk.Chunk.EndOffset, cr.parser.Pos()+rc.cfg.Mydumper.ReadBlockSize)
		if cr.parser.Pos() >= endOffset {
			break
		}

		buffer.Reset()
		start := time.Now()

		var sep byte = ' '
	readLoop:
		for cr.parser.Pos() < endOffset {
			readRowStartTime := time.Now()
			err := cr.parser.ReadRow()
			switch errors.Cause(err) {
			case nil:
				buffer.WriteByte(sep)
				if sep == ' ' {
					buffer.WriteString("INSERT INTO ")
					buffer.WriteString(t.tableName)
					if cr.chunk.Columns == nil {
						t.initializeColumns(cr.parser.Columns, cr.chunk)
					}
					buffer.Write(cr.chunk.Columns)
					buffer.WriteString(" VALUES ")
					sep = ','
				}
				metric.ChunkParserReadRowSecondsHistogram.Observe(time.Since(readRowStartTime).Seconds())
				lastRow := cr.parser.LastRow()
				if cr.chunk.ShouldIncludeRowID {
					buffer.Write(lastRow.Row[:len(lastRow.Row)-1])
					fmt.Fprintf(&buffer, ",%d)", lastRow.RowID)
				} else {
					buffer.Write(lastRow.Row)
				}
			case io.EOF:
				cr.chunk.Chunk.EndOffset = cr.parser.Pos()
				break readLoop
			default:
				return errors.Trace(err)
			}
		}
		if sep != ',' { // quick and dirty way to check if `buffer` actually contained any values
			continue
		}
		buffer.WriteByte(';')

		readDur := time.Since(start)
		readTotalDur += readDur
		metric.BlockReadSecondsHistogram.Observe(readDur.Seconds())
		metric.BlockReadBytesHistogram.Observe(float64(buffer.Len()))

		// sql -> kv
		start = time.Now()
		kvs, _, err := kvEncoder.SQL2KV(buffer.String())
		encodeDur := time.Since(start)
		encodeTotalDur += encodeDur
		metric.BlockEncodeSecondsHistogram.Observe(encodeDur.Seconds())

		common.AppLogger.Debugf("len(kvs) %d, len(sql) %d", len(kvs), buffer.Len())
		if err != nil {
			common.AppLogger.Errorf("kv encode failed = %s\n", err.Error())
			return errors.Trace(err)
		}

		block.cond.L.Lock()
		for len(block.totalKVs) > len(kvs)*maxKVQueueSize {
			// ^ hack to create a back-pressure preventing sending too many KV pairs at once
			// this happens when delivery is slower than encoding.
			// note that the KV pairs will retain the memory buffer backing the KV encoder
			// and thus blow up the memory usage and will easily cause lightning to go OOM.
			block.cond.Wait()
		}
		block.totalKVs = append(block.totalKVs, kvs...)
		block.localChecksum.Update(kvs)
		block.chunkOffset = cr.parser.Pos()
		block.chunkRowID = cr.parser.LastRow().RowID
		block.cond.Signal()
		block.cond.L.Unlock()
	}

	block.cond.L.Lock()
	block.encodeCompleted = true
	block.cond.Signal()
	block.cond.L.Unlock()

	select {
	case err := <-deliverCompleteCh:
		if err == nil {
			common.AppLogger.Infof(
				"[%s:%d] restore chunk #%d (%s) takes %v (read: %v, encode: %v, deliver: %v)",
				t.tableName, engineID, cr.index, &cr.chunk.Key, time.Since(timer),
				readTotalDur, encodeTotalDur, deliverTotalDur,
			)
		}
		return errors.Trace(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
