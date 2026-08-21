package btree

import "fmt"

// Validate walks the entire tree and checks every B-Tree invariant, returning
// the first violation found.
//
// This exists because a broken B-Tree does not usually announce itself. A tree
// with a mis-handled split still answers most queries correctly -- descent
// happens to take the right branch -- and reports a plausible height. It fails
// only for the specific keys that landed on the wrong side of a lost
// separator, which may be a handful out of a thousand.
//
// Testing Search against a reference map catches that eventually. Checking the
// invariants catches it immediately, and says which invariant broke.
//
// The invariants:
//
//  1. keys within a node are strictly ascending;
//  2. an internal node with k keys has exactly k+1 children;
//  3. every node except the root holds between t-1 and 2t-1 keys;
//     the root holds between 1 and 2t-1 (or 0 if the tree is empty);
//  4. every leaf sits at the same depth;
//  5. every key in the subtree at children[i] lies strictly between
//     keys[i-1] and keys[i] -- the separator property.
func (bt *BTree) Validate() error {
	if bt.root == nil {
		return fmt.Errorf("btree: nil root")
	}

	// Root is exempt from the minimum-key rule but not the maximum.
	if len(bt.root.keys) > 2*bt.t-1 {
		return fmt.Errorf("root holds %d keys, maximum is %d",
			len(bt.root.keys), 2*bt.t-1)
	}
	if bt.n > 0 && len(bt.root.keys) == 0 {
		return fmt.Errorf("tree holds %d keys but the root is empty", bt.n)
	}

	leafDepth := -1
	count, err := bt.root.validate(bt.t, 0, &leafDepth, nil, nil)
	if err != nil {
		return err
	}
	if count != bt.n {
		return fmt.Errorf("tree reports %d keys but %d are reachable", bt.n, count)
	}
	return nil
}

// validate checks one node and recurses. lo and hi are the exclusive bounds
// this subtree's keys must fall between, inherited from the separators above
// it; nil means unbounded on that side. It returns the number of keys in the
// subtree.
func (n *Node) validate(t, depth int, leafDepth *int, lo, hi *int) (int, error) {
	// (1) keys strictly ascending
	for i := 1; i < len(n.keys); i++ {
		if n.keys[i-1] >= n.keys[i] {
			return 0, fmt.Errorf("depth %d: keys out of order: %d >= %d at index %d",
				depth, n.keys[i-1], n.keys[i], i)
		}
	}

	// (5) every key inside the range inherited from the separators above
	for _, k := range n.keys {
		if lo != nil && k <= *lo {
			return 0, fmt.Errorf("depth %d: key %d violates lower separator %d "+
				"(separator property broken -- a median was lost on split)", depth, k, *lo)
		}
		if hi != nil && k >= *hi {
			return 0, fmt.Errorf("depth %d: key %d violates upper separator %d "+
				"(separator property broken -- a median was lost on split)", depth, k, *hi)
		}
	}

	// (3) key-count bounds, root exempted by the caller
	if depth > 0 {
		if len(n.keys) < t-1 {
			return 0, fmt.Errorf("depth %d: node holds %d keys, minimum is %d",
				depth, len(n.keys), t-1)
		}
		if len(n.keys) > 2*t-1 {
			return 0, fmt.Errorf("depth %d: node holds %d keys, maximum is %d",
				depth, len(n.keys), 2*t-1)
		}
	}

	if n.leaf {
		// (4) all leaves at the same depth
		if *leafDepth == -1 {
			*leafDepth = depth
		} else if depth != *leafDepth {
			return 0, fmt.Errorf("leaf at depth %d, but an earlier leaf was at depth %d "+
				"(tree is unbalanced)", depth, *leafDepth)
		}
		if len(n.children) != 0 {
			return 0, fmt.Errorf("depth %d: leaf has %d children", depth, len(n.children))
		}
		return len(n.keys), nil
	}

	// (2) internal node arity
	if len(n.children) != len(n.keys)+1 {
		return 0, fmt.Errorf("depth %d: node has %d keys but %d children, want %d",
			depth, len(n.keys), len(n.children), len(n.keys)+1)
	}

	total := len(n.keys)
	for i, c := range n.children {
		if c == nil {
			return 0, fmt.Errorf("depth %d: child %d is nil", depth, i)
		}
		// Child i is bounded below by keys[i-1] and above by keys[i].
		childLo, childHi := lo, hi
		if i > 0 {
			childLo = &n.keys[i-1]
		}
		if i < len(n.keys) {
			childHi = &n.keys[i]
		}
		sub, err := c.validate(t, depth+1, leafDepth, childLo, childHi)
		if err != nil {
			return 0, err
		}
		total += sub
	}
	return total, nil
}

// Fill reports the mean number of keys per node as a fraction of the maximum
// a node can hold.
//
// This is the measurement that distinguishes sequential from random insertion.
// Both produce a correctly balanced tree of near-identical height -- the
// invariants guarantee that. They do NOT produce equally well-packed trees,
// and on disk, fill factor is what determines how many pages the same data
// occupies and therefore how much I/O a scan costs.
func (bt *BTree) Fill() float64 {
	var nodes, keys int
	var walk func(*Node)
	walk = func(n *Node) {
		nodes++
		keys += len(n.keys)
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(bt.root)
	if nodes == 0 {
		return 0
	}
	return float64(keys) / float64(nodes*(2*bt.t-1))
}

// Nodes reports the total number of nodes, which on disk becomes the number of
// pages the index occupies.
func (bt *BTree) Nodes() int {
	var nodes int
	var walk func(*Node)
	walk = func(n *Node) {
		nodes++
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(bt.root)
	return nodes
}
