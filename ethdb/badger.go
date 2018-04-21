// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ethdb

import (
	//"strconv"
	//"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger/options"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/golang/snappy"
)

type BadgerDatabase struct {
	fn              string // filename for reporting
	db              *badger.DB
	getTimer        metrics.Timer // Timer for measuring the database get request counts and latencies
	putTimer        metrics.Timer // Timer for measuring the database put request counts and latencies
	delTimer        metrics.Timer // Timer for measuring the database delete request counts and latencies
	missMeter       metrics.Meter // Meter for measuring the missed database get requests
	readMeter       metrics.Meter // Meter for measuring the database get request data usage
	writeMeter      metrics.Meter // Meter for measuring the database put request data usage
	batchPutTimer   metrics.Timer
	batchWriteTimer metrics.Timer
	batchWriteMeter metrics.Meter

	quitLock sync.Mutex // Mutex protecting the quit channel access

	log log.Logger // Contextual logger tracking the database path
}

const (
	// discardRatio represents the discard ratio for the badger GC
	// https://godoc.org/github.com/dgraph-io/badger#DB.RunValueLogGC
	discardRatio = 0.5

	// GC interval
	cgInterval = 10 * time.Minute
)

// NewBadgerDatabase returns a BadgerDB wrapped object.
func NewBadgerDatabase(file string) (*BadgerDatabase, error) {
	logger := log.New("database", file)

	opts := badger.DefaultOptions
	opts.Dir = file
	opts.ValueDir = file
	opts.SyncWrites = false
	opts.ValueLogFileSize = 1 << 30
	opts.TableLoadingMode = options.MemoryMap
	db, err := badger.Open(opts)

	// (Re)check for errors and abort if opening of the db failed
	if err != nil {
		return nil, err
	}
	ret := &BadgerDatabase{
		fn:  file,
		db:  db,
		log: logger,
	}

	go ret.runGC()

	return ret, nil
}

// collectGarbage runs the garbage collection for Badger backend db
func (db *BadgerDatabase) collectGarbage() error {
	if err := db.db.PurgeOlderVersions(); err != nil {
		return err
	}

	return db.db.RunValueLogGC(discardRatio)
}

// runGC triggers the garbage collection for the Badger backend db.
// Should be run as a goroutine
func (db *BadgerDatabase) runGC() {
	ticker := time.NewTicker(cgInterval)
	for {
		select {
		case <-ticker.C:
			db.log.Info("Start garbage collecting...")
			err := db.collectGarbage()
			if err == nil {
				db.log.Info("Garbage collected")
			} else {
				db.log.Error("Failed to collect garbage", "err", err)
			}
		}
	}
}

// Path returns the path to the database directory.
func (db *BadgerDatabase) Path() string {
	return db.fn
}

// Put puts the given key / value to the queue
func (db *BadgerDatabase) Put(key []byte, value []byte) error {
	if db.putTimer != nil {
		defer db.putTimer.UpdateSince(time.Now())
	}

	if db.writeMeter != nil {
		db.writeMeter.Mark(int64(len(value)))
	}

	return db.db.Update(func(txn *badger.Txn) error {
		value = snappy.Encode(nil, value)
		err := txn.Set(key, value)
		return err
	})
}

func (db *BadgerDatabase) Has(key []byte) (ret bool, err error) {
	err = db.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if item != nil {
			ret = true
		}
		if err == badger.ErrKeyNotFound {
			ret = false
			err = nil
		}
		return err
	})
	return ret, err
}

// Get returns the given key if it's present.
func (db *BadgerDatabase) Get(key []byte) (dat []byte, err error) {
	// Measure the database get latency, if requested
	if db.getTimer != nil {
		defer db.getTimer.UpdateSince(time.Now())
	}
	err = db.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			return err
		}
		dat = common.CopyBytes(val)
		dat, err = snappy.Decode(nil, dat)
		if err != nil {
			return err
		}
		return nil
	})
	//Update the actually retrieved amount of data
	if db.readMeter != nil {
		db.readMeter.Mark(int64(len(dat)))
	}
	if err != nil {
		if db.missMeter != nil {
			db.missMeter.Mark(1)
		}
		return nil, err
	}

	return dat, nil
}

// Delete deletes the key from the queue and database
func (db *BadgerDatabase) Delete(key []byte) error {
	// Measure the database delete latency, if requested
	if db.delTimer != nil {
		defer db.delTimer.UpdateSince(time.Now())
	}
	return db.db.Update(func(txn *badger.Txn) error {
		err := txn.Delete(key)
		if err == badger.ErrKeyNotFound {
			err = nil
		}
		return err
	})
}

type badgerIterator struct {
	txn            *badger.Txn
	internIterator *badger.Iterator
	released       bool
	initialised    bool
}

func (it *badgerIterator) Release() {
	it.internIterator.Close()
	it.txn.Discard()
	it.released = true
}

func (it *badgerIterator) Released() bool {
	return it.released
}

func (it *badgerIterator) Next() bool {
	if !it.initialised {
		it.internIterator.Rewind()
		it.initialised = true
	} else {
		it.internIterator.Next()
	}
	return it.internIterator.Valid()
}

func (it *badgerIterator) Seek(key []byte) {
	it.internIterator.Seek(key)
}

func (it *badgerIterator) Key() []byte {
	return it.internIterator.Item().Key()
}

func (it *badgerIterator) Value() []byte {
	value, err := it.internIterator.Item().Value()
	if err != nil {
		return nil
	}
	return value
}

func (db *BadgerDatabase) NewIterator() badgerIterator {
	txn := db.db.NewTransaction(false)
	opts := badger.DefaultIteratorOptions
	internIterator := txn.NewIterator(opts)
	return badgerIterator{txn: txn, internIterator: internIterator, released: false, initialised: false}
}

func (db *BadgerDatabase) Close() {
	// hacky way around https://github.com/dgraph-io/badger/pull/437
	time.Sleep(time.Second * 15)
	err := db.db.Close()
	if err == nil {
		db.log.Info("Database closed")
	} else {
		db.log.Error("Failed to close database", "err", err)
	}
}

// Meter configures the database metrics collectors and
func (db *BadgerDatabase) Meter(prefix string) {
	// Short circuit metering if the metrics system is disabled
	if !metrics.Enabled {
		return
	}
	// Initialize all the metrics collector at the requested prefix
	db.getTimer = metrics.NewRegisteredTimer(prefix+"user/gets", nil)
	db.putTimer = metrics.NewRegisteredTimer(prefix+"user/puts", nil)
	db.delTimer = metrics.NewRegisteredTimer(prefix+"user/dels", nil)
	db.missMeter = metrics.NewRegisteredMeter(prefix+"user/misses", nil)
	db.readMeter = metrics.NewRegisteredMeter(prefix+"user/reads", nil)
	db.writeMeter = metrics.NewRegisteredMeter(prefix+"user/writes", nil)
	db.batchPutTimer = metrics.NewRegisteredTimer(prefix+"user/batchPuts", nil)
	db.batchWriteTimer = metrics.NewRegisteredTimer(prefix+"user/batchWriteTimes", nil)
	db.batchWriteMeter = metrics.NewRegisteredMeter(prefix+"user/batchWrites", nil)
}

func (db *BadgerDatabase) NewBatch() Batch {
	return &badgerBatch{db: db, b: make(map[string][]byte)}
}

type badgerBatch struct {
	db   *BadgerDatabase
	b    map[string][]byte
	size int
}

func (b *badgerBatch) Put(key, value []byte) error {
	if b.db.batchPutTimer != nil {
		defer b.db.batchPutTimer.UpdateSince(time.Now())
	}

	b.b[string(key)] = common.CopyBytes(value)
	b.size += len(value)
	return nil
}

func (b *badgerBatch) Write() (err error) {
	if b.db.batchWriteTimer != nil {
		defer b.db.batchWriteTimer.UpdateSince(time.Now())
	}

	if b.db.batchWriteMeter != nil {
		b.db.batchWriteMeter.Mark(int64(b.size))
	}

	err = b.db.db.Update(func(txn *badger.Txn) error {
		for key, value := range b.b {
			value = snappy.Encode(nil, value)
			err = txn.Set([]byte(key), value)
		}
		return err
	})
	b.size = 0
	b.b = make(map[string][]byte)
	return err
}

func (b *badgerBatch) Discard() {
	b.b = make(map[string][]byte)
	b.size = 0
}

func (b *badgerBatch) ValueSize() int {
	return b.size
}

func (b *badgerBatch) Reset() {
	b.b = make(map[string][]byte)
	b.size = 0
}

type table struct {
	db     Database
	prefix string
}

// NewTable returns a Database object that prefixes all keys with a given
// string.
func NewTable(db Database, prefix string) Database {
	return &table{
		db:     db,
		prefix: prefix,
	}
}

func (dt *table) Put(key []byte, value []byte) error {
	return dt.db.Put(append([]byte(dt.prefix), key...), value)
}

func (dt *table) Has(key []byte) (bool, error) {
	return dt.db.Has(append([]byte(dt.prefix), key...))
}

func (dt *table) Get(key []byte) ([]byte, error) {
	return dt.db.Get(append([]byte(dt.prefix), key...))
}

func (dt *table) Delete(key []byte) error {
	return dt.db.Delete(append([]byte(dt.prefix), key...))
}

func (dt *table) Close() {
	// Do nothing; don't close the underlying DB.
}

type tableBatch struct {
	batch  Batch
	prefix string
}

// NewTableBatch returns a Batch object which prefixes all keys with a given string.
func NewTableBatch(db Database, prefix string) Batch {
	return &tableBatch{db.NewBatch(), prefix}
}

func (dt *table) NewBatch() Batch {
	return &tableBatch{dt.db.NewBatch(), dt.prefix}
}

func (tb *tableBatch) Put(key, value []byte) error {
	return tb.batch.Put(append([]byte(tb.prefix), key...), value)
}

func (tb *tableBatch) Write() error {
	return tb.batch.Write()
}

func (tb *tableBatch) ValueSize() int {
	return tb.batch.ValueSize()
}

func (tb *tableBatch) Reset() {
	tb.batch.Reset()
}
