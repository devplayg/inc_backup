package goback

import (
	"database/sql"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	_ "github.com/mattn/go-sqlite3"
)

const (
	FileModified = 1 << iota // 1
	FileAdded    = 1 << iota // 2
	FileDeleted  = 1 << iota // 4
)

type Backup struct {
	srcDir       string
	dstDir       string
	dbOriginFile string
	dbLogFile    string
	dbFile       string
	tempDir      string
	S            *Summary
	debug        bool

	dbOrigin   *sql.DB
	dbOriginTx *sql.Tx
	dbLog      *sql.DB
	dbLogTx    *sql.Tx
}

type Summary struct {
	ID         int64
	Date       time.Time
	SrcDir     string
	DstDir     string
	State      int
	TotalSize  uint64
	TotalCount uint32

	BackupAdded    uint32
	BackupModified uint32
	BackupDeleted  uint32

	BackupSuccess uint32
	BackupFailure uint32

	BackupSize uint64
	Message    string

	ReadingTime    time.Time
	ComparisonTime time.Time
	LoggingTime    time.Time
	ExecutionTime  float64
}

func newSummary(lastId int64, srcDir string) *Summary {
	return &Summary{
		ID:     lastId,
		Date:   time.Now(),
		SrcDir: srcDir,
		State:  1,
	}
}

type File struct {
	Path    string
	Size    int64
	ModTime time.Time
	Result  int
	State   int
	Message string
}

func newFile(path string, size int64, modTime time.Time) *File {
	return &File{
		Path:    path,
		Size:    size,
		ModTime: modTime,
	}

}

func NewBackup(srcDir, dstDir string, debug bool) *Backup {
	b := Backup{
		srcDir:       filepath.Clean(srcDir),
		dstDir:       filepath.Clean(dstDir),
		dbOriginFile: filepath.Join(filepath.Clean(dstDir), "backup_origin.db"),
		dbLogFile:    filepath.Join(filepath.Clean(dstDir), "backup_log.db"),
		debug:        debug,
	}
	return &b
}

// Initialize
func (b *Backup) Initialize() error {
	var err error
	err = b.initDir()
	if err != nil {
		return err
	}

	err = b.initDB()
	if err != nil {
		return err
	}

	if b.debug {
		log.SetLevel(log.DebugLevel)
	}

	b.S = newSummary(0, b.srcDir)

	return nil
}

// Initialize directories
func (b *Backup) initDir() error {
	if _, err := os.Stat(b.srcDir); os.IsNotExist(err) {
		return err
	}

	if _, err := os.Stat(b.dstDir); os.IsNotExist(err) {
		return err
	}

	tempDir, err := ioutil.TempDir(b.dstDir, "bak")
	if err != nil {
		return err
	}
	b.tempDir = tempDir

	return nil
}

// Initialize database
func (b *Backup) initDB() error {
	var err error
	var query string

	// Set databases
	b.dbOrigin, err = sql.Open("sqlite3", b.dbOriginFile)
	if err != nil {
		return err
	}
	b.dbOriginTx, _ = b.dbOrigin.Begin()
	b.dbLog, err = sql.Open("sqlite3", b.dbLogFile)
	if err != nil {
		return err
	}
	b.dbLogTx, _ = b.dbLog.Begin()

	// Original database
	query = `
		CREATE TABLE IF NOT EXISTS bak_origin (
			path text not null,
			size int not null,
			mtime text not null
		);
	`
	_, err = b.dbOrigin.Exec(query)
	if err != nil {
		return err
	}

	// Log database
	query = `
		CREATE TABLE IF NOT EXISTS bak_summary (
			id integer not null primary key autoincrement,
			date integer not null  DEFAULT CURRENT_TIMESTAMP,
			src_dir text not null default '',
			dst_dir text not null default '',
			state integer not null default 0,
			total_size integer not null default 0,
			total_count integer not null default 0,
			backup_modified integer not null default 0,
			backup_added integer not null default 0,
			backup_deleted integer not null default 0,
			backup_success integer not null default 0,
			backup_failure integer not null default 0,
			backup_size integer not null default 0,
			execution_time real not null default 0.0,
			message text not null default ''
		);

		CREATE INDEX IF NOT EXISTS ix_bak_summary ON bak_summary(date);

		CREATE TABLE IF NOT EXISTS bak_log(
			id int not null,
			path text not null,
			size int not null,
			mtime text not null,
			state int not null,
			message text not null
		);

		CREATE INDEX IF NOT EXISTS ix_bak_log_id on bak_log(id);
`
	_, err = b.dbLog.Exec(query)
	if err != nil {
		return err
	}

	return nil
}

func (b *Backup) getOriginMap(summary *Summary) (sync.Map, int) {
	m := sync.Map{}
	if summary.ID < 1 {
		log.Info("this is first backup")
		return m, 0
	}
	log.Infof("recent backup: %s", summary.Date)

	//The most recent backup was completed on May 5.
	// Recent backups were processed on May 5th.
	rows, err := b.dbOrigin.Query("select path, size, mtime from bak_origin")
	checkErr(err)

	var count = 0
	var path string
	var size int64
	var modTime string
	for rows.Next() {
		f := newFile("", 0, time.Now())
		err = rows.Scan(&path, &size, &modTime)
		checkErr(err)
		f.Path = path
		f.Size = size
		f.ModTime, _ = time.Parse(time.RFC3339, modTime)
		m.Store(path, f)
		count += 1
	}
	return m, count
}

func (b *Backup) Start() error {
	log.Infof("source directory: %s", b.srcDir)

	// Load last backup data
	lastSummary := b.getLastSummary()
	originMap, originCount := b.getOriginMap(lastSummary)

	// Write initial data to database
	newMap := sync.Map{}
	if originCount < 1 || b.srcDir != lastSummary.SrcDir {
		b.S.State = 2
		b.S.Message = "collecting initialize data"
		log.Info(b.S.Message)

		err := filepath.Walk(b.srcDir, func(path string, f os.FileInfo, err error) error {
			if !f.IsDir() {
				fi := newFile(path, f.Size(), f.ModTime())
				newMap.Store(path, fi)
				b.S.TotalCount += 1
				b.S.TotalSize += uint64(f.Size())
			}
			return nil
		})
		checkErr(err)
		os.RemoveAll(b.tempDir)
		b.S.ReadingTime = time.Now()
		b.S.ComparisonTime = b.S.ReadingTime

		log.Infof("writing initial data")
		b.writeToDatabase(newMap, sync.Map{})
		b.S.LoggingTime = time.Now()
		return nil
	}
	b.S.ReadingTime = time.Now()

	// Search files and compare with previous data
	log.Infof("comparing old and new")
	b.S.State = 3
	i := 1
	err := filepath.Walk(b.srcDir, func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() && f.Mode().IsRegular() {

			log.Debugf("Start checking: [%d] %s (%d)", i, path, f.Size())
			atomic.AddUint32(&b.S.TotalCount, 1)
			atomic.AddUint64(&b.S.TotalSize, uint64(f.Size()))
			fi := newFile(path, f.Size(), f.ModTime())

			if inf, ok := originMap.Load(path); ok {
				last := inf.(*File)

				if last.ModTime.Unix() != f.ModTime().Unix() || last.Size != f.Size() {
					log.Debugf("modified: %s", path)
					fi.State = FileModified
					atomic.AddUint32(&b.S.BackupModified, 1)
					backupPath, dur, err := b.BackupFile(path)
					if err != nil {
						atomic.AddUint32(&b.S.BackupFailure, 1)
						log.Error(err)
						fi.Message = err.Error()
						fi.State = fi.State * -1
						//spew.Dump(fi)
					} else {
						fi.Message = fmt.Sprintf("copy_time=%4.1f", dur)
						atomic.AddUint32(&b.S.BackupSuccess, 1)
						atomic.AddUint64(&b.S.BackupSize, uint64(f.Size()))
						os.Chtimes(backupPath, f.ModTime(), f.ModTime())
						originMap.Delete(path)
					}
				}
				originMap.Delete(path)
			} else {
				log.Debugf("added: %s", path)
				fi.State = FileAdded
				atomic.AddUint32(&b.S.BackupAdded, 1)
				backupPath, dur, err := b.BackupFile(path)
				if err != nil {
					atomic.AddUint32(&b.S.BackupFailure, 1)
					log.Error(err)
					fi.Message = err.Error()
					fi.State = fi.State * -1
					//spew.Dump(fi)
				} else {
					fi.Message = fmt.Sprintf("copy_time=%4.1f", dur)
					atomic.AddUint32(&b.S.BackupSuccess, 1)
					atomic.AddUint64(&b.S.BackupSize, uint64(f.Size()))
					os.Chtimes(backupPath, f.ModTime(), f.ModTime())
				}
			}
			//if fi.State < 0 {
			//	log.Debugf("[%d] %s", fi.State, fi.Path)
			//}
			newMap.Store(path, fi)
			i++
		}
		return nil
	})
	checkErr(err)

	// Rename directory
	lastDir := filepath.Join(b.dstDir, b.S.Date.Format("20060102"))
	err = os.Rename(b.tempDir, lastDir)
	if err == nil {
		b.S.DstDir = lastDir
	} else {

		i := 1
		for err != nil && i <= 10 {
			altDir := lastDir + "_" + strconv.Itoa(i)
			err = os.Rename(b.tempDir, altDir)
			if err == nil {
				b.S.DstDir = altDir
			}
			i += 1
		}
		if err != nil {
			b.S.Message = err.Error()
			b.S.State = -1
			b.S.DstDir = b.tempDir
			os.RemoveAll(b.tempDir)
			return err
		}
	}
	b.S.ComparisonTime = time.Now()

	// Write data to database
	err = b.writeToDatabase(newMap, originMap)
	b.S.LoggingTime = time.Now()
	return err
}

func (b *Backup) getLastSummary() *Summary {
	log.Info("checking last backup data")

	rows, _ := b.dbLog.Query(`
		select id, date, src_dir
		from bak_summary
		where id = (select max(id) from bak_summary)
	`)
	defer rows.Close()

	var lastId int64
	var date string
	var srcDir string
	rows.Next()
	rows.Scan(&lastId, &date, &srcDir)

	s := newSummary(lastId, srcDir)
	s.Date, _ = time.Parse(time.RFC3339, date)

	return s
}

func (b *Backup) writeToDatabase(newMap sync.Map, originMap sync.Map) error {
	log.Info("writing to database")

	rs, err := b.dbLogTx.Exec("insert into bak_summary(date,src_dir,dst_dir,state,total_size,total_count,backup_modified,backup_added,backup_deleted,backup_success,backup_failure,backup_size,execution_time,message) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		b.S.Date.Format(time.RFC3339),
		b.S.SrcDir,
		b.S.DstDir,
		b.S.State,
		b.S.TotalSize,
		b.S.TotalCount,
		b.S.BackupModified,
		b.S.BackupAdded,
		b.S.BackupDeleted,
		b.S.BackupSuccess,
		b.S.BackupFailure,
		b.S.BackupSize,
		b.S.ExecutionTime,
		b.S.Message,
	)
	if err != nil {
		return err
	}

	b.S.ID, _ = rs.LastInsertId()
	log.Infof("backup_id=%d", b.S.ID)

	var maxInsertSize uint32 = 500
	var lines []string
	var eventLines []string
	var i uint32 = 0
	var j uint32 = 0

	// Delete original data
	b.dbOriginTx.Exec("delete from bak_origin")

	// Modified or added files
	newMap.Range(func(key, value interface{}) bool {
		f := value.(*File)
		path := strings.Replace(f.Path, "'", "''", -1)
		lines = append(lines, fmt.Sprintf("select '%s', %d, '%s'", path, f.Size, f.ModTime.Format(time.RFC3339)))

		i += 1

		if i%maxInsertSize == 0 || i == b.S.TotalCount {
			err := b.insertIntoOrigin(lines)
			checkErr(err)
			lines = nil
		}

		if f.State != 0 {
			eventLines = append(eventLines, fmt.Sprintf("select %d, '%s', %d, '%s', %d, '%s'", b.S.ID, path, f.Size, f.ModTime.Format(time.RFC3339), f.State, f.Message))
			j += 1

			if j%maxInsertSize == 0 {
				err := b.insertIntoLog(eventLines)
				checkErr(err)
				eventLines = nil
			}
		}
		return true
	})
	if len(eventLines) > 0 {
		err := b.insertIntoLog(eventLines)
		checkErr(err)
		eventLines = nil
	}

	// Deleted files
	eventLines = make([]string, 0)
	j = 0
	originMap.Range(func(key, value interface{}) bool {
		atomic.AddUint32(&b.S.BackupSuccess, 1)
		f := value.(*File)
		log.Debugf("deleted: %s", f.Path)
		f.State = FileDeleted
		path := strings.Replace(f.Path, "'", "''", -1)
		eventLines = append(eventLines, fmt.Sprintf("select %d, '%s', %d, '%s', %d, '%s'", b.S.ID, path, f.Size, f.ModTime.Format(time.RFC3339), f.State, f.Message))
		j += 1

		if j%maxInsertSize == 0 {
			err := b.insertIntoLog(eventLines)
			checkErr(err)
			eventLines = nil
		}
		return true
	})
	if len(eventLines) > 0 {
		err := b.insertIntoLog(eventLines)
		checkErr(err)
		eventLines = nil
	}
	atomic.AddUint32(&b.S.BackupDeleted, j)

	return nil
}

func (b *Backup) insertIntoLog(rows []string) error {
	query := fmt.Sprintf("insert into bak_log(id, path, size, mtime, state, message) %s", strings.Join(rows, " union all "))
	_, err := b.dbLogTx.Exec(query)
	return err
}

func (b *Backup) insertIntoOrigin(rows []string) error {
	query := fmt.Sprintf("insert into bak_origin(path, size, mtime) %s", strings.Join(rows, " union all "))
	_, err := b.dbOriginTx.Exec(query)
	defer func() {
		if r := recover(); r != nil {
			if err != nil {
				log.Println(query)
			}
		}
	}()
	checkErr(err)
	return err
}

func (b *Backup) Close() error {
	b.S.ExecutionTime = b.S.LoggingTime.Sub(b.S.Date).Seconds()
	b.S.Message += fmt.Sprintf("reading: %3.1fs, comparing: %3.1fs, writing: %3.1fs",
		b.S.ReadingTime.Sub(b.S.Date).Seconds(),
		b.S.ComparisonTime.Sub(b.S.ReadingTime).Seconds(),
		b.S.LoggingTime.Sub(b.S.ComparisonTime).Seconds(),
	)
	b.dbLogTx.Exec("update bak_summary set backup_deleted = ?, execution_time = ?, message = ? where id = ?",
		b.S.BackupDeleted,
		b.S.ExecutionTime,
		b.S.Message,
		b.S.ID,
	)

	b.dbLogTx.Commit()
	b.dbOriginTx.Commit()
	b.dbOrigin.Close()
	b.dbLog.Close()

	if b.S.ID > 1 { // ID 1 is about initializing data
		log.WithFields(log.Fields{
			"modified": b.S.BackupModified,
			"added":    b.S.BackupAdded,
			"deleted":  b.S.BackupDeleted,
		}).Infof("files: %d", b.S.BackupModified+b.S.BackupAdded+b.S.BackupDeleted)
		log.WithFields(log.Fields{
			"success": b.S.BackupSuccess,
			"failure": b.S.BackupFailure,
		}).Infof("backup result")
		log.Infof("backup size: %d(%s)", b.S.BackupSize, humanize.Bytes(b.S.BackupSize))
	}
	log.WithFields(log.Fields{
		"files": b.S.TotalCount,
		"size":  fmt.Sprintf("%d(%s)", b.S.TotalSize, humanize.Bytes(b.S.TotalSize)),
	}).Info("source directory")

	log.WithFields(log.Fields{
		"reading":    fmt.Sprintf("%3.1fs", b.S.ReadingTime.Sub(b.S.Date).Seconds()),
		"comparison": fmt.Sprintf("%3.1fs", b.S.ComparisonTime.Sub(b.S.ReadingTime).Seconds()),
		"writing":    fmt.Sprintf("%3.1fs", b.S.LoggingTime.Sub(b.S.ComparisonTime).Seconds()),
	}).Infof("execution time: %3.1fs", b.S.ExecutionTime)

	return nil
}

func (b *Backup) BackupFile(path string) (string, float64, error) {
	// Set source
	t := time.Now()
	from, err := os.Open(path)
	if err != nil {
		return "", time.Since(t).Seconds(), err

	}
	defer from.Close()

	// Set destination
	dst := filepath.Join(b.tempDir, path[len(b.srcDir):])
	err = os.MkdirAll(filepath.Dir(dst), 0644)
	to, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return "", time.Since(t).Seconds(), err
	}
	defer to.Close()

	// Copy
	_, err = io.Copy(to, from)
	if err != nil {
		return "", time.Since(t).Seconds(), err
	}

	return dst, time.Since(t).Seconds(), err
}

func checkErr(err error) {
	if err != nil {
		log.Errorf("[Error] %s", err.Error())
	}
}
