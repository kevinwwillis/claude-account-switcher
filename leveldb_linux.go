package main

import (
	"errors"

	"github.com/jmhodges/levigo"
)

// Local Storage is opened with the real LevelDB library (libleveldb, apt
// libleveldb-dev to build, libleveldb1d to run), the one Chromium is built
// on. Pure-Go readers mishandle the log files Chromium reuses and lose the
// newest writes.

type levelDB struct{ db *levigo.DB }

func openLocalStorage(dir string, create bool) (lsDB, error) {
	opts := levigo.NewOptions()
	defer opts.Close()
	opts.SetCreateIfMissing(create)
	opts.SetParanoidChecks(true)
	db, err := levigo.Open(dir, opts)
	if err != nil {
		return nil, err
	}
	return &levelDB{db}, nil
}

func (l *levelDB) get(key []byte) ([]byte, error) {
	ro := levigo.NewReadOptions()
	defer ro.Close()
	v, err := l.db.Get(ro, key)
	if err == nil && v == nil {
		return nil, errNotFound
	}
	return v, err
}

func (l *levelDB) write(puts map[string][]byte) error {
	wb := levigo.NewWriteBatch()
	defer wb.Close()
	for k, v := range puts {
		wb.Put([]byte(k), v)
	}
	wo := levigo.NewWriteOptions()
	defer wo.Close()
	wo.SetSync(true)
	return l.db.Write(wo, wb)
}

func (l *levelDB) close() error {
	if l.db == nil {
		return errors.New("closed twice")
	}
	l.db.Close()
	l.db = nil
	return nil
}
