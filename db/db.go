package db

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/SparrowDb/sparrowdb/cache"
	"github.com/SparrowDb/sparrowdb/db/index"
	"github.com/SparrowDb/sparrowdb/engine"
	"github.com/SparrowDb/sparrowdb/errors"
	"github.com/SparrowDb/sparrowdb/model"
	"github.com/SparrowDb/sparrowdb/slog"
	"github.com/SparrowDb/sparrowdb/util"
)

// Database holds database definitions
type Database struct {
	Descriptor DatabaseDescriptor
	commitlog  *Commitlog
	dhList     []dataHolder
	cache      *cache.Cache
	mu         sync.RWMutex

	compFinish chan bool
}

type dataHolder struct {
	path        string
	sto         engine.Storage
	summary     index.Summary
	bloomfilter util.BloomFilter
}

func newDataHolder(sto *engine.Storage, dbPath string, bloomFilterFp float32) (*dataHolder, error) {
	var err error

	// commitlog full path
	cPath := filepath.Join(dbPath, FolderCommitlog)

	// new name for commitlog folder
	uTime := fmt.Sprintf("%v", time.Now().UnixNano())
	newPath := filepath.Join(dbPath, uTime)

	// Rename commitlog file to data file
	if err := (*sto).Rename(engine.FileDesc{Type: engine.FileCommitlog}, engine.FileDesc{Type: engine.FileData}); err != nil {
		return nil, err
	}

	// Rename directory to unix time
	if err := os.Rename(cPath, newPath); err != nil {
		return nil, err
	}

	// Load dataholder
	dh := dataHolder{path: newPath}
	if dh.sto, err = engine.OpenFile(newPath); err != nil {
		return nil, err
	}

	// Load index from dataholder
	ir := newIndexReader(&dh.sto)
	dh.summary, err = ir.LoadIndex()
	if err != nil {
		return nil, err
	}

	// Create and populate bloomfilter
	table := dh.summary.GetTable()
	dh.bloomfilter = util.NewBloomFilter(dh.summary.Count(), bloomFilterFp)
	for _, v := range table {
		dh.bloomfilter.Add(strconv.Itoa(int(v.Key)))
	}

	bfw, err := dh.sto.Create(engine.FileDesc{Type: engine.FileBloomFilter})
	if err != nil {
		return nil, err
	}

	writer := newBufWriter(bfw)
	b := dh.bloomfilter.ByteStream()
	if err = writer.Append(b.Bytes()); err == nil {
		writer.Close()
	}

	return &dh, nil
}

func openDataHolder(path string) (*dataHolder, error) {
	var err error

	dh := dataHolder{path: path}

	dh.sto, err = engine.OpenFile(path)
	if err != nil {
		return nil, err
	}

	// Loads index
	ir := newIndexReader(&dh.sto)
	dh.summary, err = ir.LoadIndex()
	if err != nil {
		return nil, err
	}

	// Loads bloomfilter
	var pos int64
	var bfreader io.Reader

	bfreader, err = dh.sto.Open(engine.FileDesc{Type: engine.FileBloomFilter})
	if err != nil {
		return nil, err
	}

	r := newReader(bfreader.(io.ReaderAt))

	if b, err := r.Read(pos); err == nil {
		bs := util.NewByteStreamFromBytes(b)
		dh.bloomfilter = *util.NewBloomFilterFromByteStream(bs)
	}

	return &dh, nil
}

func (d *dataHolder) Get(position int64) (*util.ByteStream, error) {
	// Search in index if found, get from data file
	freader, err := d.sto.Open(engine.FileDesc{Type: engine.FileData})
	if err != nil {
		slog.Errorf(errors.ErrFileCorrupted.Error(), d.path)
		return nil, nil
	}

	r := newReader(freader.(io.ReaderAt))

	// If found key but can't load it from file, it will return nil to avoid
	// db crash. Returning nil will send to user empty query result
	b, err := r.Read(position)
	if err != nil {
		slog.Errorf(errors.ErrFileCorrupted.Error(), d.path)
		return nil, nil
	}

	bs := util.NewByteStreamFromBytes(b)
	return bs, nil
}

// InsertData insert data into database
func (db *Database) InsertData(df *model.DataDefinition) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	hKey := util.Hash32(df.Key)
	bs := df.ToByteStream()

	// Put in cache
	db.cache.Put(hKey, bs.Bytes())

	// Get last position in commitlog
	size, err := db.commitlog.Size()
	if err != nil {
		return err
	}

	// Check if commitlog has the max file size
	if size+int64(df.Size) > int64(db.Descriptor.MaxDataLogSize) {
		ndh, err := newDataHolder(&db.commitlog.sto, db.Descriptor.Path, db.Descriptor.BloomFilterFp)
		if err != nil {
			return err
		}

		db.dhList = append(db.dhList, *ndh)
		db.commitlog = NewCommitLog(db.Descriptor.Path)
	}

	if err = db.commitlog.Add(df.Key, df.Status, bs); err != nil {
		return err
	}

	return nil
}

// GetDataByKey returns pointer to DataDefinition and bool if found the data
func (db *Database) GetDataByKey(key string) (*model.DataDefinition, bool) {
	defer func() {
		if x := recover(); x != nil {
		}
	}()

	hkey := util.Hash32(key)

	// Search for given key in cache
	if c := db.cache.Get(hkey); c != nil {
		bs := util.NewByteStreamFromBytes(c)
		return model.NewDataDefinitionFromByteStream(bs), true
	}

	// Search in commitlog
	if bs := db.commitlog.Get(key); bs != nil {
		db.cache.Put(hkey, bs.Bytes())
		return model.NewDataDefinitionFromByteStream(bs), true
	}

	// Search in data files
	strKey := strconv.Itoa(int(hkey))
	dhListLen := len(db.dhList) - 1

	// Search from the newest dataholder until the oldest
	for curr := dhListLen; curr >= 0; curr-- {
		if db.dhList[curr].bloomfilter.Contains(strKey) {
			if e, eIdx := db.dhList[curr].summary.LookUp(hkey); eIdx == true {
				bs, err := db.dhList[curr].Get(e.Offset)
				if err != nil {
					return nil, false
				}
				return model.NewDataDefinitionFromByteStream(bs), true
			}
		}
	}

	return nil, false
}

// LoadData loads index and bloom filter from each data file
func (db *Database) LoadData() {
	flist, _ := ioutil.ReadDir(db.Descriptor.Path)
	for _, v := range flist {
		if m, _ := regexp.MatchString("^([0-9]{19})$", v.Name()); m == true {
			dh, err := openDataHolder(filepath.Join(db.Descriptor.Path, v.Name()))
			if err != nil {
				slog.Fatalf(err.Error())
			}
			db.dhList = append(db.dhList, *dh)
		}
	}
}

func (db *Database) compactionNotification() {
	slog.Infof("%s compaction started: %s", db.Descriptor.Name, time.Now())
	select {
	case <-db.compFinish:
		slog.Infof("%s compaction finished: %s", db.Descriptor.Name, time.Now())
	}
}

// Close closes databases
func (db *Database) Close() {
	// removes db from compaction service
	removeDbCompaction(db.Descriptor.Name)
}

// NewDatabase returns new Database
func NewDatabase(descriptor DatabaseDescriptor) *Database {
	db := Database{
		Descriptor: descriptor,
		commitlog:  NewCommitLog(descriptor.Path),
		cache:      cache.NewCache(cache.NewLRU(int64(descriptor.MaxCacheSize))),

		compFinish: make(chan bool),
	}

	// add database in compaction service
	registerDbCompaction(&db)

	return &db
}

// OpenDatabase returns oppened Database
func OpenDatabase(descriptor DatabaseDescriptor) *Database {
	db := NewDatabase(descriptor)
	db.commitlog.LoadData()
	db.LoadData()
	return db
}
