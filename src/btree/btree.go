// Package btree implements an in-memory B-Tree, as the first half of Phase 3.
//
// SCOPE. This package deliberately stops short of disk. Keys are plain ints,
// nodes are ordinary Go structs linked by pointers, and nothing is serialised.
// Pages, the buffer pool and on-disk node layout are Session 7; putting them
// here would mean debugging the tree and the storage format at the same time,
// which is how both end up wrong.
//
// WHY A B-TREE AT ALL. Phase 2 (Bitcask) answers a read in one seek by keeping
// every key in RAM. Two consequences follow, and this package exists because of
// them:
//
//   - the key set must fit in memory, which bounds the store by RAM rather
//     than by disk;
//   - the keydir is a hash map, so keys have no order, and a range scan
//     ("every key between A and B") is not slow -- it is impossible without a
//     full scan of the log.
//
// A B-Tree gives up Bitcask's one-seek read to buy both back: the index lives
// on disk, and it is sorted.
package btree

import "sort"

// Node is one B-Tree node.
//
// A node holds up to 2t-1 keys in sorted order and, if it is internal, exactly
// one more child than it has keys. Child i covers the key range below keys[i];
// child i+1 covers the range above it. The keys in an internal node are
// therefore SEPARATORS, not merely data: each one is the boundary between the
// two subtrees on either side of it.
type Node struct {
	keys     []int
	children []*Node
	leaf     bool
}

// BTree is a B-Tree of minimum degree t.
//
// The minimum degree is the single parameter that defines the shape:
//
//	keys per node:     min t-1,  max 2t-1   (the root may hold fewer)
//	children per node: min t,    max 2t     (internal nodes only)
//
// Session 6 uses t=2, which yields nodes of 1-3 keys and 2-4 children -- the
// structure usually called a 2-3-4 tree. t=2 is chosen precisely because it is
// the SMALLEST legal degree, so splits happen constantly and any bug in the
// split logic surfaces within a handful of insertions instead of hiding until
// the tree is large.
//
// On disk the calculus reverses completely: t is chosen so that one node fills
// exactly one page, which for 4 KiB pages puts t in the hundreds. That is
// Session 7's problem, and it is why this file keeps t a variable rather than
// baking in 2.
type BTree struct {
	root *Node
	t    int
	n    int // number of distinct keys stored
}

// New returns an empty B-Tree of minimum degree t. It panics for t < 2, since
// t=1 would permit nodes with zero keys and the structure would not be a tree.
func New(t int) *BTree {
	if t < 2 {
		panic("btree: minimum degree must be at least 2")
	}
	return &BTree{
		root: &Node{leaf: true},
		t:    t,
	}
}

// Len reports how many distinct keys the tree holds.
func (bt *BTree) Len() int { return bt.n }

// Height reports the number of levels, counting the root as level 1. An empty
// tree has height 1: a single empty leaf.
//
// Because every leaf in a B-Tree sits at the same depth -- the invariant the
// whole structure exists to maintain -- descending through any single path
// measures the height of the entire tree.
func (bt *BTree) Height() int {
	h := 1
	for n := bt.root; !n.leaf; n = n.children[0] {
		h++
	}
	return h
}

// Search reports whether key is present.
//
// At each node a binary search finds the first key not less than the target.
// Either it matches, or it identifies the child whose range contains the
// target. The walk is therefore one node per level, and the height bounds the
// work: on disk, the height IS the number of page reads, which is the entire
// reason B-Trees are shaped wide and shallow.
func (bt *BTree) Search(key int) bool {
	n := bt.root
	for {
		i := sort.SearchInts(n.keys, key)
		if i < len(n.keys) && n.keys[i] == key {
			return true
		}
		if n.leaf {
			return false
		}
		// keys[i] is the first key greater than the target, so the target
		// belongs in the subtree to its left -- which is child i.
		n = n.children[i]
	}
}

// full reports whether the node has the maximum 2t-1 keys and must be split
// before anything more can be inserted beneath it.
func (n *Node) full(t int) bool { return len(n.keys) == 2*t-1 }

// Insert adds key to the tree. It returns false if the key was already
// present, in which case the tree is unchanged.
//
// DUPLICATE HANDLING. The membership check is a separate descent, which costs
// a second traversal. That is a deliberate trade of speed for clarity: fusing
// the check into the inserting descent is possible, but the preemptive splits
// below move keys between nodes as the walk proceeds, so the fused version has
// to reason about a tree that is changing shape underneath it. A production
// tree does exactly that; this one is optimised to be read by a jury.
func (bt *BTree) Insert(key int) bool {
	if bt.Search(key) {
		return false
	}

	root := bt.root
	if root.full(bt.t) {
		// THE ONLY PLACE A B-TREE GROWS TALLER.
		//
		// Every other split pushes its median into an existing parent, which
		// leaves the height unchanged. When the root itself is full there is
		// no parent to receive the median, so a new one is created above it.
		// This is why all leaves stay at the same depth: the tree does not
		// grow at the bottom, it grows at the top, lifting every leaf by one
		// level simultaneously.
		newRoot := &Node{leaf: false, children: []*Node{root}}
		bt.root = newRoot
		newRoot.splitChild(0, bt.t)
		newRoot.insertNonFull(key, bt.t)
	} else {
		root.insertNonFull(key, bt.t)
	}

	bt.n++
	return true
}

// splitChild splits the full child at index i of n, which must not itself be
// full.
//
// The full child holds 2t-1 keys. They divide as:
//
//	keys[0 : t-1]   stay in the original node (t-1 keys)
//	keys[t-1]       the MEDIAN -- moves UP into n
//	keys[t : 2t-1]  move to a new right sibling (t-1 keys)
//
// WHY THE MEDIAN MOVES UP. This is the crux of the entire structure, and the
// answer is that it is the only choice that preserves the separator property.
// The parent's keys are not data that happens to live there; each one is the
// boundary between the subtrees on either side of it. After a split there are
// two subtrees where there was one, so the parent needs one more boundary
// between them -- and the only value that is simultaneously greater than
// everything on the left and less than everything on the right is the median.
//
// Keeping the median in either half would leave the parent with two adjacent
// children and no separator between them, and a subsequent search would have
// no basis on which to choose one. The tree would still LOOK correct on
// sequential keys, because descent almost always heads the same direction; it
// corrupts on random keys, which is why the height experiment in the tests
// inserts both.
func (n *Node) splitChild(i, t int) {
	full := n.children[i]

	right := &Node{leaf: full.leaf}

	// COPY, DO NOT SUB-SLICE. `right.keys = full.keys[t:]` would alias one
	// backing array between two nodes: a later append to full.keys would write
	// straight into right.keys. Same reasoning as DecodeRecord in the bitcask
	// package -- copying is the boring, correct choice.
	right.keys = make([]int, t-1)
	copy(right.keys, full.keys[t:])

	if !full.leaf {
		right.children = make([]*Node, t)
		copy(right.children, full.children[t:])
	}

	median := full.keys[t-1]

	// Truncate the original. The capacity beyond the new length still belongs
	// to full, and nothing else references it now that the copies are made.
	full.keys = full.keys[:t-1]
	if !full.leaf {
		full.children = full.children[:t]
	}

	// Insert the median at position i and the new sibling at position i+1, so
	// that the separator sits between the two halves it separates.
	n.keys = append(n.keys, 0)
	copy(n.keys[i+1:], n.keys[i:])
	n.keys[i] = median

	n.children = append(n.children, nil)
	copy(n.children[i+2:], n.children[i+1:])
	n.children[i+1] = right
}

// insertNonFull inserts key into the subtree rooted at n, which is guaranteed
// not to be full.
//
// PREEMPTIVE SPLITTING. The descent splits any full child BEFORE stepping into
// it, rather than inserting first and splitting back up the tree afterwards.
// The precondition -- n is never full when we arrive -- is what makes this
// safe: there is always room for a median arriving from below.
//
// The payoff is that a split never propagates. Each insertion is a single
// downward pass with no recursion back toward the root, no parent pointers,
// and no case where a split cascades several levels up. On disk that matters
// more than it does here: an upward-propagating split would require holding
// write latches on every node along the path.
func (n *Node) insertNonFull(key, t int) {
	i := sort.SearchInts(n.keys, key)

	if n.leaf {
		// All insertions land in a leaf. Internal nodes only ever acquire keys
		// by receiving a median pushed up from a split.
		n.keys = append(n.keys, 0)
		copy(n.keys[i+1:], n.keys[i:])
		n.keys[i] = key
		return
	}

	if n.children[i].full(t) {
		n.splitChild(i, t)
		// The split pushed a median up into n.keys[i], so the child that was
		// at i is now two children, at i and i+1. Compare against the median
		// to learn which side the key belongs on.
		if key > n.keys[i] {
			i++
		}
	}

	n.children[i].insertNonFull(key, t)
}

// Keys returns every key in the tree in ascending order.
//
// This is the capability Bitcask cannot offer at any price. The keydir is a
// hash map, so producing sorted keys there means reading every record in the
// log and sorting the result. Here it is an in-order walk, and the same walk
// bounded by two endpoints is a range scan.
func (bt *BTree) Keys() []int {
	out := make([]int, 0, bt.n)
	var walk func(*Node)
	walk = func(n *Node) {
		for i := range n.keys {
			if !n.leaf {
				walk(n.children[i])
			}
			out = append(out, n.keys[i])
		}
		if !n.leaf {
			walk(n.children[len(n.keys)])
		}
	}
	walk(bt.root)
	return out
}
