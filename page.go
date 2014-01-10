package bolt

import (
	"unsafe"
)

const maxPageSize = 0x8000

var _page page
const headerSize = unsafe.Offsetof(_page.ptr)

const minPageKeys = 2
const fillThreshold = 250 // 25%

const (
	p_branch   = 0x01
	p_leaf     = 0x02
	p_overflow = 0x04
	p_meta     = 0x08
	p_dirty    = 0x10 /**< dirty page, also set for #P_SUBP pages */
	p_sub      = 0x40
	p_keep     = 0x8000 /**< leave this page alone during spill */
)

// maxCommitPages is the maximum number of pages to commit in one writev() call.
const maxCommitPages 64

/* max bytes to write in one call */
const maxWriteByteCount 0x80000000U    // TODO: #define MAX_WRITE 0x80000000U >> (sizeof(ssize_t) == 4))

// TODO:
// #if defined(IOV_MAX) && IOV_MAX < MDB_COMMIT_PAGES
// #undef MDB_COMMIT_PAGES
// #define MDB_COMMIT_PAGES	IOV_MAX
// #endif

// TODO: #define MDB_PS_MODIFY	1
// TODO: #define MDB_PS_ROOTONLY	2
// TODO: #define MDB_PS_FIRST	4
// TODO: #define MDB_PS_LAST		8

// TODO: #define MDB_SPLIT_REPLACE	MDB_APPENDDUP	/**< newkey is not new */

type pgno uint64

type page struct {
	id       pgno
	flags    int
	lower    int
	upper    int
	overflow int
	ptr      int
}

type pageState struct {
	head int  /**< Reclaimed freeDB pages, or NULL before use */
	last int  /**< ID of last used record, or 0 if !mf_pghead */
}

// meta returns a pointer to the metadata section of the page.
func (p *page) meta() (*meta, error) {
	// Exit if page is not a meta page.
	if (p.flags & p_meta) != 0 {
		return InvalidMetaPageError
	}

	// Cast the meta section and validate before returning.
	m := (*meta)(unsafe.Pointer(&p.ptr))
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}








// nodeCount returns the number of nodes on the page.
func (p *page) nodeCount() int {
	return 0 // (p.header.lower - unsafe.Sizeof(p.header) >> 1
}

// remainingSize returns the number of bytes left in the page.
func (p *page) remainingSize() int {
	return p.header.upper - p.header.lower
}

// remainingSize returns the number of bytes left in the page.
func (p *page) remainingSize() int {
	return p.header.upper - p.header.lower
}
