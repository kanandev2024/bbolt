package bolt

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// Ensure that a node can insert a key/value.
func TestNodePut(t *testing.T) {
	n := &node{inodes: make(inodes, 0)}
	n.put([]byte("baz"), []byte("baz"), []byte("2"), 0)
	n.put([]byte("foo"), []byte("foo"), []byte("0"), 0)
	n.put([]byte("bar"), []byte("bar"), []byte("1"), 0)
	n.put([]byte("foo"), []byte("foo"), []byte("3"), 0)
	assert.Equal(t, len(n.inodes), 3)
	assert.Equal(t, n.inodes[0].key, []byte("bar"))
	assert.Equal(t, n.inodes[0].value, []byte("1"))
	assert.Equal(t, n.inodes[1].key, []byte("baz"))
	assert.Equal(t, n.inodes[1].value, []byte("2"))
	assert.Equal(t, n.inodes[2].key, []byte("foo"))
	assert.Equal(t, n.inodes[2].value, []byte("3"))
}

// Ensure that a node can deserialize from a leaf page.
func TestNodeReadLeafPage(t *testing.T) {
	// Create a page.
	var buf [4096]byte
	page := (*page)(unsafe.Pointer(&buf[0]))
	page.flags = leafPageFlag
	page.count = 2

	// Insert 2 elements at the beginning. sizeof(leafPageElement) == 16
	nodes := (*[3]leafPageElement)(unsafe.Pointer(&page.ptr))
	nodes[0] = leafPageElement{flags: 0, pos: 32, ksize: 3, vsize: 4}  // pos = sizeof(leafPageElement) * 2
	nodes[1] = leafPageElement{flags: 0, pos: 23, ksize: 10, vsize: 3} // pos = sizeof(leafPageElement) + 3 + 4

	// Write data for the nodes at the end.
	data := (*[4096]byte)(unsafe.Pointer(&nodes[2]))
	copy(data[:], []byte("barfooz"))
	copy(data[7:], []byte("helloworldbye"))

	// Deserialize page into a leaf.
	n := &node{}
	n.read(page)

	// Check that there are two inodes with correct data.
	assert.True(t, n.isLeaf)
	assert.Equal(t, len(n.inodes), 2)
	assert.Equal(t, n.inodes[0].key, []byte("bar"))
	assert.Equal(t, n.inodes[0].value, []byte("fooz"))
	assert.Equal(t, n.inodes[1].key, []byte("helloworld"))
	assert.Equal(t, n.inodes[1].value, []byte("bye"))
}

// Ensure that a node can serialize into a leaf page.
func TestNodeWriteLeafPage(t *testing.T) {
	// Create a node.
	n := &node{isLeaf: true, inodes: make(inodes, 0)}
	n.put([]byte("susy"), []byte("susy"), []byte("que"), 0)
	n.put([]byte("ricki"), []byte("ricki"), []byte("lake"), 0)
	n.put([]byte("john"), []byte("john"), []byte("johnson"), 0)

	// Write it to a page.
	var buf [4096]byte
	p := (*page)(unsafe.Pointer(&buf[0]))
	n.write(p)

	// Read the page back in.
	n2 := &node{}
	n2.read(p)

	// Check that the two pages are the same.
	assert.Equal(t, len(n2.inodes), 3)
	assert.Equal(t, n2.inodes[0].key, []byte("john"))
	assert.Equal(t, n2.inodes[0].value, []byte("johnson"))
	assert.Equal(t, n2.inodes[1].key, []byte("ricki"))
	assert.Equal(t, n2.inodes[1].value, []byte("lake"))
	assert.Equal(t, n2.inodes[2].key, []byte("susy"))
	assert.Equal(t, n2.inodes[2].value, []byte("que"))
}

// Ensure that a node can split into appropriate subgroups.
func TestNodeSplit(t *testing.T) {
	// Create a node.
	n := &node{inodes: make(inodes, 0)}
	n.put([]byte("00000001"), []byte("00000001"), []byte("0123456701234567"), 0)
	n.put([]byte("00000002"), []byte("00000002"), []byte("0123456701234567"), 0)
	n.put([]byte("00000003"), []byte("00000003"), []byte("0123456701234567"), 0)
	n.put([]byte("00000004"), []byte("00000004"), []byte("0123456701234567"), 0)
	n.put([]byte("00000005"), []byte("00000005"), []byte("0123456701234567"), 0)

	// Split between 2 & 3.
	nodes := n.split(100)

	assert.Equal(t, len(nodes), 2)
	assert.Equal(t, len(nodes[0].inodes), 2)
	assert.Equal(t, len(nodes[1].inodes), 3)
}

// Ensure that a page with the minimum number of inodes just returns a single node.
func TestNodeSplitWithMinKeys(t *testing.T) {
	// Create a node.
	n := &node{inodes: make(inodes, 0)}
	n.put([]byte("00000001"), []byte("00000001"), []byte("0123456701234567"), 0)
	n.put([]byte("00000002"), []byte("00000002"), []byte("0123456701234567"), 0)

	// Split.
	nodes := n.split(20)
	assert.Equal(t, len(nodes), 1)
	assert.Equal(t, len(nodes[0].inodes), 2)
}

// Ensure that a node that has keys that all fit on a page just returns one leaf.
func TestNodeSplitFitsInPage(t *testing.T) {
	// Create a node.
	n := &node{inodes: make(inodes, 0)}
	n.put([]byte("00000001"), []byte("00000001"), []byte("0123456701234567"), 0)
	n.put([]byte("00000002"), []byte("00000002"), []byte("0123456701234567"), 0)
	n.put([]byte("00000003"), []byte("00000003"), []byte("0123456701234567"), 0)
	n.put([]byte("00000004"), []byte("00000004"), []byte("0123456701234567"), 0)
	n.put([]byte("00000005"), []byte("00000005"), []byte("0123456701234567"), 0)

	// Split.
	nodes := n.split(4096)
	assert.Equal(t, len(nodes), 1)
	assert.Equal(t, len(nodes[0].inodes), 5)
}
