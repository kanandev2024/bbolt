package bolt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// The smallest size that the mmap can be.
const minMmapSize = 1 << 22 // 4MB

// The largest step that can be taken when remapping the mmap.
const maxMmapStep = 1 << 30 // 1GB

var (
	// ErrDatabaseNotOpen is returned when a DB instance is accessed before it
	// is opened or after it is closed.
	ErrDatabaseNotOpen = errors.New("database not open")

	// ErrDatabaseOpen is returned when opening a database that is
	// already open.
	ErrDatabaseOpen = errors.New("database already open")
)

// DB represents a collection of buckets persisted to a file on disk.
// All data access is performed through transactions which can be obtained through the DB.
// All the functions on DB will return a ErrDatabaseNotOpen if accessed before Open() is called.
type DB struct {
	path     string
	file     *os.File
	data     []byte
	meta0    *meta
	meta1    *meta
	pageSize int
	opened   bool
	rwtx     *Tx
	txs      []*Tx
	freelist *freelist
	stats    Stats

	rwlock   sync.Mutex   // Allows only one writer at a time.
	metalock sync.Mutex   // Protects meta page access.
	mmaplock sync.RWMutex // Protects mmap access during remapping.

	ops struct {
		writeAt func(b []byte, off int64) (n int, err error)
	}
}

// Path returns the path to currently open database file.
func (db *DB) Path() string {
	return db.path
}

// GoString returns the Go string representation of the database.
func (db *DB) GoString() string {
	return fmt.Sprintf("bolt.DB{path:%q}", db.path)
}

// String returns the string representation of the database.
func (db *DB) String() string {
	return fmt.Sprintf("DB<%q>", db.path)
}

// Open creates and opens a database at the given path.
// If the file does not exist then it will be created automatically.
func Open(path string, mode os.FileMode) (*DB, error) {
	var db = &DB{opened: true}

	// Open data file and separate sync handler for metadata writes.
	db.path = path

	var err error
	if db.file, err = os.OpenFile(db.path, os.O_RDWR|os.O_CREATE, mode); err != nil {
		_ = db.close()
		return nil, err
	}

	// Lock file so that other processes using Bolt cannot use the database
	// at the same time. This would cause corruption since the two processes
	// would write meta pages and free pages separately.
	if err := syscall.Flock(int(db.file.Fd()), syscall.LOCK_EX); err != nil {
		_ = db.close()
		return nil, err
	}

	// Default values for test hooks
	db.ops.writeAt = db.file.WriteAt

	// Initialize the database if it doesn't exist.
	if info, err := db.file.Stat(); err != nil {
		return nil, fmt.Errorf("stat error: %s", err)
	} else if info.Size() == 0 {
		// Initialize new files with meta pages.
		if err := db.init(); err != nil {
			return nil, err
		}
	} else {
		// Read the first meta page to determine the page size.
		var buf [0x1000]byte
		if _, err := db.file.ReadAt(buf[:], 0); err == nil {
			m := db.pageInBuffer(buf[:], 0).meta()
			if err := m.validate(); err != nil {
				return nil, fmt.Errorf("meta error: %s", err)
			}
			db.pageSize = int(m.pageSize)
		}
	}

	// Memory map the data file.
	if err := db.mmap(0); err != nil {
		_ = db.close()
		return nil, err
	}

	// Read in the freelist.
	db.freelist = &freelist{pending: make(map[txid][]pgid)}
	db.freelist.read(db.page(db.meta().freelist))

	// Mark the database as opened and return.
	return db, nil
}

// mmap opens the underlying memory-mapped file and initializes the meta references.
// minsz is the minimum size that the new mmap can be.
func (db *DB) mmap(minsz int) error {
	db.mmaplock.Lock()
	defer db.mmaplock.Unlock()

	// Dereference all mmap references before unmapping.
	if db.rwtx != nil {
		db.rwtx.dereference()
	}

	// Unmap existing data before continuing.
	if err := db.munmap(); err != nil {
		return err
	}

	info, err := db.file.Stat()
	if err != nil {
		return fmt.Errorf("mmap stat error: %s", err)
	} else if int(info.Size()) < db.pageSize*2 {
		return fmt.Errorf("file size too small")
	}

	// Ensure the size is at least the minimum size.
	var size = int(info.Size())
	if size < minsz {
		size = minsz
	}
	size = db.mmapSize(size)

	// Memory-map the data file as a byte slice.
	if db.data, err = syscall.Mmap(int(db.file.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED); err != nil {
		return err
	}

	// Save references to the meta pages.
	db.meta0 = db.page(0).meta()
	db.meta1 = db.page(1).meta()

	// Validate the meta pages.
	if err := db.meta0.validate(); err != nil {
		return fmt.Errorf("meta0 error: %s", err)
	}
	if err := db.meta1.validate(); err != nil {
		return fmt.Errorf("meta1 error: %s", err)
	}

	return nil
}

// munmap unmaps the data file from memory.
func (db *DB) munmap() error {
	if db.data != nil {
		if err := syscall.Munmap(db.data); err != nil {
			return fmt.Errorf("unmap error: " + err.Error())
		}
		db.data = nil
	}
	return nil
}

// mmapSize determines the appropriate size for the mmap given the current size
// of the database. The minimum size is 4MB and doubles until it reaches 1GB.
func (db *DB) mmapSize(size int) int {
	if size < minMmapSize {
		return minMmapSize
	} else if size < maxMmapStep {
		size *= 2
	} else {
		size += maxMmapStep
	}

	// Ensure that the mmap size is a multiple of the page size.
	if (size % db.pageSize) != 0 {
		size = ((size / db.pageSize) + 1) * db.pageSize
	}

	return size
}

// init creates a new database file and initializes its meta pages.
func (db *DB) init() error {
	// Set the page size to the OS page size.
	db.pageSize = os.Getpagesize()

	// Create two meta pages on a buffer.
	buf := make([]byte, db.pageSize*4)
	for i := 0; i < 2; i++ {
		p := db.pageInBuffer(buf[:], pgid(i))
		p.id = pgid(i)
		p.flags = metaPageFlag

		// Initialize the meta page.
		m := p.meta()
		m.magic = magic
		m.version = version
		m.pageSize = uint32(db.pageSize)
		m.version = version
		m.freelist = 2
		m.buckets = 3
		m.pgid = 4
		m.txid = txid(i)
	}

	// Write an empty freelist at page 3.
	p := db.pageInBuffer(buf[:], pgid(2))
	p.id = pgid(2)
	p.flags = freelistPageFlag
	p.count = 0

	// Write an empty leaf page at page 4.
	p = db.pageInBuffer(buf[:], pgid(3))
	p.id = pgid(3)
	p.flags = bucketsPageFlag
	p.count = 0

	// Write the buffer to our data file.
	if _, err := db.ops.writeAt(buf, 0); err != nil {
		return err
	}
	if err := fdatasync(db.file); err != nil {
		return err
	}

	return nil
}

// Close releases all database resources.
// All transactions must be closed before closing the database.
func (db *DB) Close() error {
	db.metalock.Lock()
	defer db.metalock.Unlock()
	return db.close()
}

func (db *DB) close() error {
	db.opened = false

	db.freelist = nil
	db.path = ""

	// Clear ops.
	db.ops.writeAt = nil

	// Close the mmap.
	if err := db.munmap(); err != nil {
		return err
	}

	// Close file handles.
	if db.file != nil {
		// Unlock the file.
		_ = syscall.Flock(int(db.file.Fd()), syscall.LOCK_UN)

		// Close the file descriptor.
		if err := db.file.Close(); err != nil {
			return fmt.Errorf("db file close: %s", err)
		}
		db.file = nil
	}

	return nil
}

// Begin starts a new transaction.
// Multiple read-only transactions can be used concurrently but only one
// write transaction can be used at a time. Starting multiple write transactions
// will cause the calls to block and be serialized until the current write
// transaction finishes.
//
// IMPORTANT: You must close read-only transactions after you are finished or
// else the database will not reclaim old pages.
func (db *DB) Begin(writable bool) (*Tx, error) {
	if writable {
		return db.beginRWTx()
	}
	return db.beginTx()
}

func (db *DB) beginTx() (*Tx, error) {
	db.metalock.Lock()
	defer db.metalock.Unlock()

	// Obtain a read-only lock on the mmap. When the mmap is remapped it will
	// obtain a write lock so all transactions must finish before it can be
	// remapped.
	db.mmaplock.RLock()

	// Exit if the database is not open yet.
	if !db.opened {
		return nil, ErrDatabaseNotOpen
	}

	// Create a transaction associated with the database.
	t := &Tx{}
	t.init(db)

	// Keep track of transaction until it closes.
	db.txs = append(db.txs, t)

	return t, nil
}

func (db *DB) beginRWTx() (*Tx, error) {
	db.metalock.Lock()
	defer db.metalock.Unlock()

	// Obtain writer lock. This is released by the transaction when it closes.
	db.rwlock.Lock()

	// Exit if the database is not open yet.
	if !db.opened {
		db.rwlock.Unlock()
		return nil, ErrDatabaseNotOpen
	}

	// Create a transaction associated with the database.
	t := &Tx{writable: true}
	t.init(db)
	db.rwtx = t

	// Free any pages associated with closed read-only transactions.
	var minid txid = 0xFFFFFFFFFFFFFFFF
	for _, t := range db.txs {
		if t.id() < minid {
			minid = t.id()
		}
	}
	if minid > 0 {
		db.freelist.release(minid - 1)
	}

	return t, nil
}

// removeTx removes a transaction from the database.
func (db *DB) removeTx(t *Tx) {
	db.metalock.Lock()
	defer db.metalock.Unlock()

	// Release the read lock on the mmap.
	db.mmaplock.RUnlock()

	// Remove the transaction.
	for i, tx := range db.txs {
		if tx == t {
			db.txs = append(db.txs[:i], db.txs[i+1:]...)
			break
		}
	}

	// Merge statistics.
	db.stats.TxStats.add(&t.stats)
}

// Update executes a function within the context of a read-write managed transaction.
// If no error is returned from the function then the transaction is committed.
// If an error is returned then the entire transaction is rolled back.
// Any error that is returned from the function or returned from the commit is
// returned from the Update() method.
//
// Attempting to manually commit or rollback within the function will cause a panic.
func (db *DB) Update(fn func(*Tx) error) error {
	t, err := db.Begin(true)
	if err != nil {
		return err
	}

	// Mark as a managed tx so that the inner function cannot manually commit.
	t.managed = true

	// If an error is returned from the function then rollback and return error.
	err = fn(t)
	t.managed = false
	if err != nil {
		_ = t.Rollback()
		return err
	}

	return t.Commit()
}

// View executes a function within the context of a managed read-only transaction.
// Any error that is returned from the function is returned from the View() method.
//
// Attempting to manually rollback within the function will cause a panic.
func (db *DB) View(fn func(*Tx) error) error {
	t, err := db.Begin(false)
	if err != nil {
		return err
	}

	// Mark as a managed tx so that the inner function cannot manually rollback.
	t.managed = true

	// If an error is returned from the function then pass it through.
	err = fn(t)
	t.managed = false
	if err != nil {
		_ = t.Rollback()
		return err
	}

	if err := t.Rollback(); err != nil {
		return err
	}

	return nil
}

// Copy writes the entire database to a writer.
// A reader transaction is maintained during the copy so it is safe to continue
// using the database while a copy is in progress.
func (db *DB) Copy(w io.Writer) error {
	// Maintain a reader transaction so pages don't get reclaimed.
	t, err := db.Begin(false)
	if err != nil {
		return err
	}

	// Open reader on the database.
	f, err := os.Open(db.path)
	if err != nil {
		_ = t.Rollback()
		return err
	}

	// Copy the meta pages.
	db.metalock.Lock()
	_, err = io.CopyN(w, f, int64(db.pageSize*2))
	db.metalock.Unlock()
	if err != nil {
		_ = t.Rollback()
		_ = f.Close()
		return fmt.Errorf("meta copy: %s", err)
	}

	// Copy data pages.
	if _, err := io.Copy(w, f); err != nil {
		_ = t.Rollback()
		_ = f.Close()
		return err
	}

	// Close read transaction and exit.
	if err := t.Rollback(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// CopyFile copies the entire database to file at the given path.
// A reader transaction is maintained during the copy so it is safe to continue
// using the database while a copy is in progress.
func (db *DB) CopyFile(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	err = db.Copy(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Stats retrieves ongoing performance stats for the database.
// This is only updated when a transaction closes.
func (db *DB) Stats() Stats {
	db.metalock.Lock()
	defer db.metalock.Unlock()
	return db.stats
}

// Check performs several consistency checks on the database.
// An error is returned if any inconsistency is found.
func (db *DB) Check() error {
	return db.Update(func(tx *Tx) error {
		var errors ErrorList

		// Track every reachable page.
		reachable := make(map[pgid]*page)
		reachable[0] = tx.page(0) // meta0
		reachable[1] = tx.page(1) // meta1
		for i := uint32(0); i <= tx.page(tx.meta.buckets).overflow; i++ {
			reachable[tx.meta.buckets+pgid(i)] = tx.page(tx.meta.buckets)
		}
		for i := uint32(0); i <= tx.page(tx.meta.freelist).overflow; i++ {
			reachable[tx.meta.freelist+pgid(i)] = tx.page(tx.meta.freelist)
		}

		// Check each reachable page within each bucket.
		for _, bucket := range tx.Buckets() {
			// warnf("[bucket] %s", bucket.name)
			tx.forEachPage(bucket.root, 0, func(p *page, _ int) {
				// Ensure each page is only referenced once.
				for i := pgid(0); i <= pgid(p.overflow); i++ {
					var id = p.id + i
					if _, ok := reachable[id]; ok {
						errors = append(errors, fmt.Errorf("page %d: multiple references", int(id)))
					}
					reachable[id] = p
				}

				// Retrieve page info.
				info, err := tx.Page(int(p.id))
				// warnf("[page] %d + %d (%s)", p.id, p.overflow, info.Type)
				if err != nil {
					errors = append(errors, err)
				} else if info == nil {
					errors = append(errors, fmt.Errorf("page %d: out of bounds: %d", int(p.id), int(tx.meta.pgid)))
				} else if info.Type != "branch" && info.Type != "leaf" {
					errors = append(errors, fmt.Errorf("page %d: invalid type: %s", int(p.id), info.Type))
				}
			})
		}

		// Ensure all pages below high water mark are either reachable or freed.
		for i := pgid(0); i < tx.meta.pgid; i++ {
			_, isReachable := reachable[i]
			if !isReachable && !db.freelist.isFree(i) {
				errors = append(errors, fmt.Errorf("page %d: unreachable unfreed", int(i)))
			}
		}

		// TODO(benbjohnson): Ensure that only one buckets page exists.

		if len(errors) > 0 {
			return errors
		}

		return nil
	})
}

// page retrieves a page reference from the mmap based on the current page size.
func (db *DB) page(id pgid) *page {
	pos := id * pgid(db.pageSize)
	return (*page)(unsafe.Pointer(&db.data[pos]))
}

// pageInBuffer retrieves a page reference from a given byte array based on the current page size.
func (db *DB) pageInBuffer(b []byte, id pgid) *page {
	return (*page)(unsafe.Pointer(&b[id*pgid(db.pageSize)]))
}

// meta retrieves the current meta page reference.
func (db *DB) meta() *meta {
	if db.meta0.txid > db.meta1.txid {
		return db.meta0
	}
	return db.meta1
}

// allocate returns a contiguous block of memory starting at a given page.
func (db *DB) allocate(count int) (*page, error) {
	// Allocate a temporary buffer for the page.
	buf := make([]byte, count*db.pageSize)
	p := (*page)(unsafe.Pointer(&buf[0]))
	p.overflow = uint32(count - 1)

	// Use pages from the freelist if they are available.
	if p.id = db.freelist.allocate(count); p.id != 0 {
		return p, nil
	}

	// Resize mmap() if we're at the end.
	p.id = db.rwtx.meta.pgid
	var minsz = int((p.id+pgid(count))+1) * db.pageSize
	if minsz >= len(db.data) {
		if err := db.mmap(minsz); err != nil {
			return nil, fmt.Errorf("mmap allocate error: %s", err)
		}
	}

	// Move the page id high water mark.
	db.rwtx.meta.pgid += pgid(count)

	return p, nil
}

// Stats represents statistics about the database.
type Stats struct {
	TxStats TxStats // global, ongoing stats.
}

// Sub calculates and returns the difference between two sets of database stats.
// This is useful when obtaining stats at two different points and time and
// you need the performance counters that occurred within that time span.
func (s *Stats) Sub(other *Stats) Stats {
	var diff Stats
	diff.TxStats = s.TxStats.Sub(&other.TxStats)
	return diff
}

func (s *Stats) add(other *Stats) {
	s.TxStats.add(&other.TxStats)
}
