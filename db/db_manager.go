package db

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sparrowdb/model"
	"github.com/sparrowdb/slog"
	"github.com/sparrowdb/util"
)

var (
	// ErrCreateDb error message when create database
	ErrCreateDb = errors.New("Could not create database")

	// ErrDropDb error message when drop database
	ErrDropDb = errors.New("Could not drop database")

	// ErrOpenDb error message when open database
	ErrOpenDb = errors.New("Could not open database")
)

// DBManager holds all databases
type DBManager struct {
	Config         *SparrowConfig
	databases      map[string]*Database
	databaseConfig *DatabaseConfig
	lock           sync.RWMutex
}

func (dbm *DBManager) checkAndFillDescriptor(descriptor *DatabaseDescriptor) {
	if len(strings.TrimSpace(descriptor.Path)) == 0 {
		descriptor.Path = filepath.Join(dbm.Config.Path, descriptor.Name)
	}
	if len(strings.TrimSpace(descriptor.Mode)) == 0 {
		descriptor.Mode = dbm.Config.Mode
	}
	if len(strings.TrimSpace(descriptor.CronExp)) == 0 {
		descriptor.CronExp = dbm.Config.CronExp
	}

	if descriptor.BloomFilterFp <= 0 {
		descriptor.BloomFilterFp = dbm.Config.BloomFilterFp
	}
	if descriptor.MaxCacheSize <= 0 {
		descriptor.MaxCacheSize = dbm.Config.MaxCacheSize
	}
	if descriptor.MaxDataLogSize <= 0 {
		descriptor.MaxDataLogSize = dbm.Config.MaxDataLogSize
	}
}

// CreateDatabase create database
func (dbm *DBManager) CreateDatabase(descriptor DatabaseDescriptor) error {
	dbm.lock.RLock()
	defer dbm.lock.RUnlock()

	_, ok := dbm.GetDatabase(descriptor.Name)

	if !ok {
		// check in descriptor wich values must be set
		// as default value
		dbm.checkAndFillDescriptor(&descriptor)

		// create dir for the database with configured path
		err := util.CreateDir(descriptor.Path)

		if err != nil {
			return ErrCreateDb
		}

		dbm.databases[descriptor.Name] = NewDatabase(&descriptor)
		dbm.databaseConfig.SaveDatabase(descriptor)

		return nil
	}

	return ErrCreateDb
}

// DropDatabase drop database
func (dbm *DBManager) DropDatabase(dbname string) error {
	dbm.lock.RLock()
	defer dbm.lock.RUnlock()

	db, ok := dbm.GetDatabase(dbname)

	if ok {
		exists, err := util.Exists(db.Descriptor.Path)

		if err != nil {
			return ErrDropDb
		}

		if exists {
			delete(dbm.databases, dbname)
			util.DeleteDir(db.Descriptor.Path)
			dbm.databaseConfig.DropDatabase(dbname)
		}

		return nil
	}

	return ErrDropDb
}

// GetDatabase returns database by database name
func (dbm *DBManager) GetDatabase(dbname string) (*Database, bool) {
	value, ok := dbm.databases[dbname]
	return value, ok
}

// GetData returns pointer to DataDefinition and bool if found the data
func (dbm *DBManager) GetData(dbname string, strKey string) <-chan *model.DataDefinition {
	result := make(chan *model.DataDefinition)
	go dbm.getData(dbname, strKey, result)
	return result
}

func (dbm *DBManager) getData(dbname string, strKey string, result chan *model.DataDefinition) {
	defer close(result)

	db, hasDb := dbm.GetDatabase(dbname)

	if hasDb {
		data, ret := db.GetDataByKey(util.Hash32(strKey))

		if ret {
			result <- data
		} else {
			result <- nil
		}
	}
}

// GetDatabasesNames returns all databases names
func (dbm *DBManager) GetDatabasesNames() []string {
	keys := make([]string, 0, len(dbm.databases))
	for k := range dbm.databases {
		keys = append(keys, k)
	}
	return keys
}

// LoadDatabases loads databases from disk
func (dbm *DBManager) LoadDatabases() {
	var buffer bytes.Buffer
	descriptors := dbm.databaseConfig.LoadDatabases()

	for _, d := range descriptors {
		_, err := dbm.openDatabase(&d)

		if err != nil {
			slog.Fatalf("Erro trying to load %s: %s\n[%s]\n\nQuiting...", d.Name, err, string(d.ToJSON()))
			os.Exit(1)
		}

		buffer.WriteString(d.Name + " ")
	}

	slog.Infof("Databases loaded: %s", buffer.String())
}

func (dbm *DBManager) openDatabase(descriptor *DatabaseDescriptor) (*Database, error) {
	// Check database directory
	exists, _ := util.Exists(descriptor.Path)
	if !exists {
		return nil, fmt.Errorf("%s: %s", ErrOpenDb, descriptor.Name)
	}

	database := OpenDatabase(descriptor)

	dbm.databases[descriptor.Name] = database

	return database, nil
}

// NewDBManager returns new DBManager
func NewDBManager(config *SparrowConfig, dbConfig *DatabaseConfig) *DBManager {
	dbm := DBManager{
		Config:         config,
		databases:      make(map[string]*Database),
		databaseConfig: dbConfig,
	}
	return &dbm
}