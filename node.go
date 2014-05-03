package bolt

import (
	"bytes"
	"sort"
	"unsafe"
)

// node represents an in-memory, deserialized page.
type node struct {
	bucket     *Bucket
	isLeaf     bool
	unbalanced bool
	key        []byte
	pgid       pgid
	parent     *node
	children   []*node
	inodes     inodes
}

// root returns the top-level node this node is attached to.
func (n *node) root() *node {
	if n.parent == nil {
		return n
	}
	return n.parent.root()
}

// minKeys returns the minimum number of inodes this node should have.
func (n *node) minKeys() int {
	if n.isLeaf {
		return 1
	}
	return 2
}

// size returns the size of the node after serialization.
func (n *node) size() int {
	var elementSize = n.pageElementSize()

	var size = pageHeaderSize
	for _, item := range n.inodes {
		size += elementSize + len(item.key) + len(item.value)
	}
	return size
}

// pageElementSize returns the size of each page element based on the type of node.
func (n *node) pageElementSize() int {
	if n.isLeaf {
		return leafPageElementSize
	}
	return branchPageElementSize
}

// childAt returns the child node at a given index.
func (n *node) childAt(index int) *node {
	_assert(!n.isLeaf, "invalid childAt(%d) on a leaf node", index)
	return n.bucket.node(n.inodes[index].pgid, n)
}

// childIndex returns the index of a given child node.
func (n *node) childIndex(child *node) int {
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].key, child.key) != -1 })
	return index
}

// numChildren returns the number of children.
func (n *node) numChildren() int {
	return len(n.inodes)
}

// nextSibling returns the next node with the same parent.
func (n *node) nextSibling() *node {
	if n.parent == nil {
		return nil
	}
	index := n.parent.childIndex(n)
	if index >= n.parent.numChildren()-1 {
		return nil
	}
	return n.parent.childAt(index + 1)
}

// prevSibling returns the previous node with the same parent.
func (n *node) prevSibling() *node {
	if n.parent == nil {
		return nil
	}
	index := n.parent.childIndex(n)
	if index == 0 {
		return nil
	}
	return n.parent.childAt(index - 1)
}

// put inserts a key/value.
func (n *node) put(oldKey, newKey, value []byte, pgid pgid, flags uint32) {
	// Find insertion index.
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].key, oldKey) != -1 })

	// Add capacity and shift nodes if we don't have an exact match and need to insert.
	exact := (len(n.inodes) > 0 && index < len(n.inodes) && bytes.Equal(n.inodes[index].key, oldKey))
	if !exact {
		n.inodes = append(n.inodes, inode{})
		copy(n.inodes[index+1:], n.inodes[index:])
	}

	inode := &n.inodes[index]
	inode.flags = flags
	inode.key = newKey
	inode.value = value
	inode.pgid = pgid
}

// del removes a key from the node.
func (n *node) del(key []byte) {
	// Find index of key.
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].key, key) != -1 })

	// Exit if the key isn't found.
	if index >= len(n.inodes) || !bytes.Equal(n.inodes[index].key, key) {
		return
	}

	// Delete inode from the node.
	n.inodes = append(n.inodes[:index], n.inodes[index+1:]...)

	// Mark the node as needing rebalancing.
	n.unbalanced = true
}

// read initializes the node from a page.
func (n *node) read(p *page) {
	n.pgid = p.id
	n.isLeaf = ((p.flags & leafPageFlag) != 0)
	n.inodes = make(inodes, int(p.count))

	for i := 0; i < int(p.count); i++ {
		inode := &n.inodes[i]
		if n.isLeaf {
			elem := p.leafPageElement(uint16(i))
			inode.flags = elem.flags
			inode.key = elem.key()
			inode.value = elem.value()
		} else {
			elem := p.branchPageElement(uint16(i))
			inode.pgid = elem.pgid
			inode.key = elem.key()
		}
	}

	// Save first key so we can find the node in the parent when we spill.
	if len(n.inodes) > 0 {
		n.key = n.inodes[0].key
	} else {
		n.key = nil
	}
}

// write writes the items onto one or more pages.
func (n *node) write(p *page) {
	// Initialize page.
	if n.isLeaf {
		p.flags |= leafPageFlag
	} else {
		p.flags |= branchPageFlag
	}
	p.count = uint16(len(n.inodes))

	// Loop over each item and write it to the page.
	b := (*[maxAllocSize]byte)(unsafe.Pointer(&p.ptr))[n.pageElementSize()*len(n.inodes):]
	for i, item := range n.inodes {
		// Write the page element.
		if n.isLeaf {
			elem := p.leafPageElement(uint16(i))
			elem.pos = uint32(uintptr(unsafe.Pointer(&b[0])) - uintptr(unsafe.Pointer(elem)))
			elem.flags = item.flags
			elem.ksize = uint32(len(item.key))
			elem.vsize = uint32(len(item.value))
		} else {
			elem := p.branchPageElement(uint16(i))
			elem.pos = uint32(uintptr(unsafe.Pointer(&b[0])) - uintptr(unsafe.Pointer(elem)))
			elem.ksize = uint32(len(item.key))
			elem.pgid = item.pgid
		}

		// Write data for the element to the end of the page.
		copy(b[0:], item.key)
		b = b[len(item.key):]
		copy(b[0:], item.value)
		b = b[len(item.value):]
	}
}

// split breaks up a node into smaller nodes, if appropriate.
// This should only be called from the spill() function.
func (n *node) split(pageSize int) []*node {
	var nodes = []*node{n}

	// Ignore the split if the page doesn't have at least enough nodes for
	// multiple pages or if the data can fit on a single page.
	if len(n.inodes) <= (minKeysPerPage*2) || n.size() < pageSize {
		return nodes
	}

	// Set fill threshold to 50%.
	threshold := pageSize / 2

	// Group into smaller pages and target a given fill size.
	size := pageHeaderSize
	internalNodes := n.inodes
	current := n
	current.inodes = nil

	// Loop over every inode and split once we reach our threshold.
	for i, inode := range internalNodes {
		elemSize := n.pageElementSize() + len(inode.key) + len(inode.value)

		// Split once we reach our threshold split size. However, this should
		// only be done if we have enough keys for this node and we will have
		// enough keys for the next node.
		if len(current.inodes) >= minKeysPerPage && i < len(internalNodes)-minKeysPerPage && size+elemSize > threshold {
			// If there's no parent then we need to create one.
			if n.parent == nil {
				n.parent = &node{bucket: n.bucket, children: []*node{n}}
			}

			// Create a new node and add it to the parent.
			current = &node{bucket: n.bucket, isLeaf: n.isLeaf, parent: n.parent}
			n.parent.children = append(n.parent.children, current)
			nodes = append(nodes, current)

			// Reset our running total back to zero (plus header size).
			size = pageHeaderSize

			// Update the statistics.
			n.bucket.tx.stats.Split++
		}

		// Increase our running total of the size and append the inode.
		size += elemSize
		current.inodes = append(current.inodes, inode)
	}

	return nodes
}

// spill writes the nodes to dirty pages and splits nodes as it goes.
// Returns an error if dirty pages cannot be allocated.
func (n *node) spill() error {
	var tx = n.bucket.tx

	// Spill child nodes first.
	for _, child := range n.children {
		if err := child.spill(); err != nil {
			return err
		}
	}

	// Add node's page to the freelist if it's not new.
	if n.pgid > 0 {
		tx.db.freelist.free(tx.id(), tx.page(n.pgid))
	}

	// Spill nodes by deepest first.
	var nodes = n.split(tx.db.pageSize)
	for _, node := range nodes {
		// Allocate contiguous space for the node.
		p, err := tx.allocate((node.size() / tx.db.pageSize) + 1)
		if err != nil {
			return err
		}

		// Write the node.
		node.write(p)
		node.pgid = p.id

		// Insert into parent inodes.
		if node.parent != nil {
			var key = node.key
			if key == nil {
				key = node.inodes[0].key
			}

			node.parent.put(key, node.inodes[0].key, nil, node.pgid, 0)
			node.key = node.inodes[0].key
		}

		// Update the statistics.
		tx.stats.Spill++
	}

	// This is a special case where we need to write the parent if it is new
	// and caused by a split in the root.
	var parent = n.parent
	if parent != nil && parent.pgid == 0 {
		// Allocate contiguous space for the node.
		p, err := tx.allocate((parent.size() / tx.db.pageSize) + 1)
		if err != nil {
			return err
		}

		// Write the new root.
		parent.write(p)
		parent.pgid = p.id
	}

	return nil
}

// rebalance attempts to combine the node with sibling nodes if the node fill
// size is below a threshold or if there are not enough keys.
func (n *node) rebalance() {
	if !n.unbalanced {
		return
	}
	n.unbalanced = false

	// Update statistics.
	n.bucket.tx.stats.Rebalance++

	// Ignore if node is above threshold (25%) and has enough keys.
	var threshold = n.bucket.tx.db.pageSize / 4
	if n.size() > threshold && len(n.inodes) > n.minKeys() {
		return
	}

	// Root node has special handling.
	if n.parent == nil {
		// If root node is a branch and only has one node then collapse it.
		if !n.isLeaf && len(n.inodes) == 1 {
			// Move root's child up.
			child := n.bucket.nodes[n.inodes[0].pgid]
			n.isLeaf = child.isLeaf
			n.inodes = child.inodes[:]
			n.children = child.children

			// Reparent all child nodes being moved.
			for _, inode := range n.inodes {
				if child, ok := n.bucket.nodes[inode.pgid]; ok {
					child.parent = n
				}
			}

			// Remove old child.
			child.parent = nil
			delete(n.bucket.nodes, child.pgid)
			child.free()
		}

		return
	}

	_assert(n.parent.numChildren() > 1, "parent must have at least 2 children")

	// Destination node is right sibling if idx == 0, otherwise left sibling.
	var target *node
	var useNextSibling = (n.parent.childIndex(n) == 0)
	if useNextSibling {
		target = n.nextSibling()
	} else {
		target = n.prevSibling()
	}

	// If target node has extra nodes then just move one over.
	if target.numChildren() > target.minKeys() {
		if useNextSibling {
			// Reparent and move node.
			if child, ok := n.bucket.nodes[target.inodes[0].pgid]; ok {
				child.parent.removeChild(child)
				child.parent = n
				child.parent.children = append(child.parent.children, child)
			}
			n.inodes = append(n.inodes, target.inodes[0])
			target.inodes = target.inodes[1:]

			// Update target key on parent.
			target.parent.put(target.key, target.inodes[0].key, nil, target.pgid, 0)
			target.key = target.inodes[0].key
		} else {
			// Reparent and move node.
			if child, ok := n.bucket.nodes[target.inodes[len(target.inodes)-1].pgid]; ok {
				child.parent.removeChild(child)
				child.parent = n
				child.parent.children = append(child.parent.children, child)
			}
			n.inodes = append(n.inodes, inode{})
			copy(n.inodes[1:], n.inodes)
			n.inodes[0] = target.inodes[len(target.inodes)-1]
			target.inodes = target.inodes[:len(target.inodes)-1]
		}

		// Update parent key for node.
		n.parent.put(n.key, n.inodes[0].key, nil, n.pgid, 0)
		n.key = n.inodes[0].key

		return
	}

	// If both this node and the target node are too small then merge them.
	if useNextSibling {
		// Reparent all child nodes being moved.
		for _, inode := range target.inodes {
			if child, ok := n.bucket.nodes[inode.pgid]; ok {
				child.parent.removeChild(child)
				child.parent = n
				child.parent.children = append(child.parent.children, child)
			}
		}

		// Copy over inodes from target and remove target.
		n.inodes = append(n.inodes, target.inodes...)
		n.parent.del(target.key)
		n.parent.removeChild(target)
		delete(n.bucket.nodes, target.pgid)
		target.free()
	} else {
		// Reparent all child nodes being moved.
		for _, inode := range n.inodes {
			if child, ok := n.bucket.nodes[inode.pgid]; ok {
				child.parent.removeChild(child)
				child.parent = target
				child.parent.children = append(child.parent.children, child)
			}
		}

		// Copy over inodes to target and remove node.
		target.inodes = append(target.inodes, n.inodes...)
		n.parent.del(n.key)
		n.parent.removeChild(n)
		n.parent.put(target.key, target.inodes[0].key, nil, target.pgid, 0)
		delete(n.bucket.nodes, n.pgid)
		n.free()
	}

	// Either this node or the target node was deleted from the parent so rebalance it.
	n.parent.rebalance()
}

// removes a node from the list of in-memory children.
// This does not affect the inodes.
func (n *node) removeChild(target *node) {
	for i, child := range n.children {
		if child == target {
			n.children = append(n.children[:i], n.children[i+1:]...)
			return
		}
	}
}

// dereference causes the node to copy all its inode key/value references to heap memory.
// This is required when the mmap is reallocated so inodes are not pointing to stale data.
func (n *node) dereference() {
	key := make([]byte, len(n.key))
	copy(key, n.key)
	n.key = key

	for i := range n.inodes {
		inode := &n.inodes[i]

		key := make([]byte, len(inode.key))
		copy(key, inode.key)
		inode.key = key

		value := make([]byte, len(inode.value))
		copy(value, inode.value)
		inode.value = value
	}
}

// free adds the node's underlying page to the freelist.
func (n *node) free() {
	if n.pgid != 0 {
		n.bucket.tx.db.freelist.free(n.bucket.tx.id(), n.bucket.tx.page(n.pgid))
		n.pgid = 0
	}
}

// inode represents an internal node inside of a node.
// It can be used to point to elements in a page or point
// to an element which hasn't been added to a page yet.
type inode struct {
	flags uint32
	pgid  pgid
	key   []byte
	value []byte
}

type inodes []inode
