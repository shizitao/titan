package db

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"

	"gitlab.meitu.com/platform/thanos/conf"
	"gitlab.meitu.com/platform/thanos/db/store"
	"gitlab.meitu.com/platform/thanos/metrics"
)

var (
	// ErrTypeMismatch indicates object type of key is not as expect
	ErrTypeMismatch = errors.New("type mismatch")

	// ErrKeyNotFound key not exist
	ErrKeyNotFound = errors.New("key not found")

	// ErrInteger valeu is not interge
	ErrInteger = errors.New("value is not an integer or out of range")

	// ErrPrecision list index reach precision limitatin
	ErrPrecision = errors.New("list reaches precision limitation, rebalance now")

	// ErrOutOfRange index/offset out of range
	ErrOutOfRange = errors.New("error index/offset out of range")

	// ErrInvalidLength data length is invalid for unmarshaler"
	ErrInvalidLength = errors.New("error data length is invalid for unmarshaler")

	// ErrEncodingMismatch object encoding type
	ErrEncodingMismatch = errors.New("error object encoding type")

	// IsErrNotFound returns true if the key is not found, otherwise return false
	IsErrNotFound = store.IsErrNotFound

	// IsRetryableError returns true if the error is temporary and can be retried
	IsRetryableError = store.IsRetryableError

	// IsConflictError return true if the error is conflict
	IsConflictError = store.IsConflictError

	// sysNamespace default namespace
	sysNamespace = "$sys"

	// sysDatabaseID default db id
	sysDatabaseID = 0
)

// Iterator store.Iterator
type Iterator store.Iterator

// DBID is the redis database ID
type DBID byte

// String returns the string format of DBID
func (id DBID) String() string {
	return fmt.Sprintf("%03d", id)
}

// Bytes DBID returns a byte slice
func (id DBID) Bytes() []byte {
	return []byte(id.String())
}

func toDBID(v []byte) DBID {
	id, _ := strconv.Atoi(string(v))
	return DBID(id)
}

// BatchGetValues issues batch requests to get values
func BatchGetValues(txn *Transaction, keys [][]byte) ([][]byte, error) {
	kvs, err := store.BatchGetValues(txn.t, keys)
	if err != nil {
		return nil, err
	}
	values := make([][]byte, len(keys))
	for i := range keys {
		values[i] = kvs[string(keys[i])]
	}
	return values, nil
}

// DB is a redis compatible data structure storage
type DB struct {
	Namespace string
	ID        DBID
	kv        *RedisStore
}

// RedisStore wraps store.Storage
type RedisStore struct {
	store.Storage
}

// Open a storage instance
func Open(conf *conf.Tikv) (*RedisStore, error) {
	s, err := store.Open(conf.PdAddrs)
	if err != nil {
		return nil, err
	}
	rds := &RedisStore{s}
	sysdb := rds.DB(sysNamespace, sysDatabaseID)
	go StartGC(sysdb)
	go StartExpire(sysdb)
	//go StartZT(sysdb, conf)

	return rds, nil
}

// DB returns a DB object with sepcific ID
func (rds *RedisStore) DB(namesapce string, id int) *DB {
	return &DB{Namespace: namesapce, ID: DBID(id), kv: rds}
}

// Close the storage instance
func (rds *RedisStore) Close() error {
	return rds.Close()
}

// Transaction supplies transaction for data structures
type Transaction struct {
	t  store.Transaction
	db *DB
}

// Begin a transaction
func (db *DB) Begin() (*Transaction, error) {
	txn, err := db.kv.Begin()
	if err != nil {
		return nil, err
	}
	return &Transaction{t: txn, db: db}, nil
}

// Prefix returns the prefix of a DB object
func (db *DB) Prefix() []byte {
	var prefix []byte
	prefix = append(prefix, []byte(db.Namespace)...)
	prefix = append(prefix, ':')
	prefix = append(prefix, db.ID.Bytes()...)
	prefix = append(prefix, ':')
	return prefix
}

// Commit a transaction
func (txn *Transaction) Commit(ctx context.Context) error {
	return txn.t.Commit(ctx)
}

// Rollback a transaction
func (txn *Transaction) Rollback() error {
	return txn.t.Rollback()
}

// List return a lists object, a new list is created if the key dose not exist.
func (txn *Transaction) List(key []byte) (List, error) {
	return GetList(txn, key)
}

// NewList returns a new list object
func (txn *Transaction) NewList(key []byte, count int) (List, error) {
	return NewList(txn, key, count)
}

// String returns a string object, but the object is unsafe, maybe the object is expire,or not exist
func (txn *Transaction) String(key []byte) (*String, error) {
	return GetString(txn, key)
}

// Strings returns a slice of String
func (txn *Transaction) Strings(keys [][]byte) ([]*String, error) {
	sobjs := make([]*String, len(keys))
	tkeys := make([][]byte, len(keys))
	for i, key := range keys {
		tkeys[i] = MetaKey(txn.db, key)
	}
	mdata, err := store.BatchGetValues(txn.t, tkeys)
	if err != nil {
		return nil, err
	}
	for i, key := range tkeys {
		obj := txn.NewString(key)
		if data, ok := mdata[string(key)]; ok {
			if err := obj.decode(data); err != nil {
				zap.L().Error("strings decode failed",
					zap.ByteString("key", key),
					zap.Error(err))
			}
		}
		sobjs[i] = obj
	}
	return sobjs, nil
}

// NewString returns a new string object
func (txn *Transaction) NewString(key []byte) *String {
	return NewString(txn, key)
}

// Kv returns a kv object
func (txn *Transaction) Kv() *Kv {
	return GetKv(txn)
}

// Hash returns a hash object
func (txn *Transaction) Hash(key []byte) (*Hash, error) {
	return GetHash(txn, key)
}

// Set returns a set object
func (txn *Transaction) Set(key []byte) (*Set, error) {
	return GetSet(txn, key)
}

// LockKeys tries to lock the entries with the keys in KV store.
func (txn *Transaction) LockKeys(keys ...[]byte) error {
	return store.LockKeys(txn.t, keys)
}

// MetaKey build to metakey from a redis key
func MetaKey(db *DB, key []byte) []byte {
	var mkey []byte
	mkey = append(mkey, []byte(db.Namespace)...)
	mkey = append(mkey, ':')
	mkey = append(mkey, db.ID.Bytes()...)
	mkey = append(mkey, ':', 'M', ':')
	mkey = append(mkey, key...)
	return mkey
}

// DataKey builds a datakey from a redis key
func DataKey(db *DB, key []byte) []byte {
	var dkey []byte
	dkey = append(dkey, []byte(db.Namespace)...)
	dkey = append(dkey, ':')
	dkey = append(dkey, db.ID.Bytes()...)
	dkey = append(dkey, ':', 'D', ':')
	dkey = append(dkey, key...)
	return dkey
}

func flushLease(txn store.Transaction, key, id []byte, interval time.Duration) error {
	databytes := make([]byte, 24)
	copy(databytes, id)
	ts := uint64((time.Now().Add(interval * time.Second).Unix()))
	binary.BigEndian.PutUint64(databytes[16:], ts)

	if err := txn.Set(key, databytes); err != nil {
		return err
	}
	return nil
}

func checkLeader(txn store.Transaction, key, id []byte, interval time.Duration) (bool, error) {
	val, err := txn.Get(key)
	if err != nil {
		if !IsErrNotFound(err) {
			zap.L().Error("query leader message faild",
				zap.ByteString("key", key),
				zap.ByteString("id", id),
				zap.Error(err))
			return false, err
		}

		zap.L().Debug("no leader now, create new lease",
			zap.ByteString("key", key),
			zap.ByteString("id", id))

		if err := flushLease(txn, key, id, interval); err != nil {
			zap.L().Error("create lease failed",
				zap.ByteString("key", key),
				zap.ByteString("id", id),
				zap.Error(err))
			return false, err
		}

		return true, nil
	}

	curID := val[0:16]
	ts := int64(binary.BigEndian.Uint64(val[16:]))

	if time.Now().Unix() > ts {
		if err := flushLease(txn, key, id, interval); err != nil {
			zap.L().Error("create lease failed",
				zap.ByteString("key", key),
				zap.ByteString("id", id),
				zap.Error(err))
			return false, err
		}
		return true, nil
	}

	if bytes.Equal(curID, id) {
		if err := flushLease(txn, key, id, interval); err != nil {
			zap.L().Error("flush lease failed",
				zap.ByteString("key", key),
				zap.ByteString("curid", curID),
				zap.ByteString("id", id),
				zap.Error(err))
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func isLeader(db *DB, leader []byte, interval time.Duration) (bool, error) {
	count := 0
	label := "default"
	switch {
	case bytes.Equal(leader, sysZTLeader):
		label = "ZT"
	case bytes.Equal(leader, sysGCLeader):
		label = "GC"
	case bytes.Equal(leader, sysExpireLeader):
		label = "EX"
	}

	for {
		txn, err := db.Begin()
		if err != nil {
			zap.L().Error("transection begin failed",
				zap.ByteString("leader", leader),
				zap.Error(err))
			continue
		}

		isLeader, err := checkLeader(txn.t, leader, UUID(), interval)
		mtFunc := func() {
			if isLeader {
				metrics.GetMetrics().IsLeaderGaugeVec.WithLabelValues(label).Set(1)
				return
			}
			metrics.GetMetrics().IsLeaderGaugeVec.WithLabelValues(label).Set(0)
		}

		if err != nil {
			txn.Rollback()
			if IsRetryableError(err) {
				count++
				if count < 3 {
					continue
				}
			}
			mtFunc()
			return isLeader, err
		}

		if err := txn.Commit(context.Background()); err != nil {
			txn.Rollback()
			if IsRetryableError(err) {
				count++
				if count < 3 {
					continue
				}
			}
			mtFunc()
			return isLeader, err
		}
		mtFunc()
		return isLeader, err
	}
}
