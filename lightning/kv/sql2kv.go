package kv

import (
	"github.com/juju/errors"
	"github.com/pingcap/tidb-lightning/lightning/datasource/base"
	sqltool "github.com/pingcap/tidb-lightning/lightning/sql"
	kvec "github.com/pingcap/tidb/util/kvencoder"
	log "github.com/sirupsen/logrus"
)

type TableKVEncoder struct {
	db          string
	table       string
	tableID     int64
	tableSchema string
	columns     int

	stmtID    uint32
	bufValues []interface{}

	encoder        kvec.KvEncoder
	idAllocator    *kvec.Allocator
	usePrepareStmt bool
}

func NewTableKVEncoder(
	db string, table string, tableID int64,
	columns int, tableSchema string, sqlMode string, idAlloc *kvec.Allocator, usePrepareStmt bool) (*TableKVEncoder, error) {

	kvEncoder, err := kvec.New(db, idAlloc)
	if err != nil {
		log.Errorf("[sql2kv] kv encoder create failed : %v", err)
		return nil, errors.Trace(err)
	}

	err = kvEncoder.SetSystemVariable("sql_mode", sqlMode)
	if err != nil {
		return nil, errors.Trace(err)
	}
	log.Debugf("set sql_mode=%s", sqlMode)

	enc := &TableKVEncoder{
		db:             db,
		table:          table,
		tableID:        tableID,
		columns:        columns,
		encoder:        kvEncoder,
		idAllocator:    idAlloc,
		tableSchema:    tableSchema,
		usePrepareStmt: usePrepareStmt,
	}

	if err = enc.init(); err != nil {
		enc.Close()
		return nil, errors.Trace(err)
	}

	return enc, nil
}

func (enc *TableKVEncoder) init() error {
	if err := enc.encoder.ExecDDLSQL(enc.tableSchema); err != nil {
		log.Errorf("[sql2kv] tableSchema execute failed : %v", err)
		return errors.Trace(err)
	}

	if enc.usePrepareStmt {
		stmtID, err := enc.makeStatements()
		if err != nil {
			return errors.Trace(err)
		}
		enc.stmtID = stmtID
	}

	return nil
}

func (enc *TableKVEncoder) makeStatements() (uint32, error) {
	return enc.prepareStatement()
}

func (enc *TableKVEncoder) ResetRowID(rowID int64) {
	enc.idAllocator.Reset(rowID)
}

func (enc *TableKVEncoder) Close() error {
	return enc.encoder.Close()
}

func (enc *TableKVEncoder) NextRowID() int64 {
	return enc.idAllocator.Base() + 1
}

func (enc *TableKVEncoder) SQL2KV(payload *base.Payload) ([]kvec.KvPair, uint64, error) {
	if enc.usePrepareStmt {
		// via prepare statement
		kvPairs, rowsAffected, err := enc.encodeViaPstmt(payload.Params)
		if err != nil {
			return nil, 0, errors.Trace(err)
		}
		return kvPairs, rowsAffected, nil
	}

	// via sql execution
	kvPairs, rowsAffected, err := enc.encoder.Encode(payload.SQL, enc.tableID)
	if err != nil {
		log.Errorf("[sql2kv] sql encode error = %v", err)
		return nil, 0, errors.Trace(err)
	}

	return kvPairs, rowsAffected, nil
}

func (enc *TableKVEncoder) encodeViaPstmt(params []interface{}) ([]kvec.KvPair, uint64, error) {
	stmtID := enc.applyStmtID()
	kvs, affected, err := enc.encoder.EncodePrepareStmt(enc.tableID, stmtID, params...)
	if err != nil {
		return nil, 0, errors.Trace(err)
	}
	return kvs, affected, nil
}

func (enc *TableKVEncoder) applyStmtID() uint32 {
	return enc.stmtID
}

func (enc *TableKVEncoder) prepareStatement() (uint32, error) {
	stmt := sqltool.MakePrepareStatement(enc.table, enc.columns, 1)
	stmtID, err := enc.encoder.PrepareStmt(stmt)
	return stmtID, errors.Trace(err)
}
