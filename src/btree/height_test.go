package btree

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// ---------------------------------------------------------------------------
// A naive unbalanced BST, for contrast only
// ---------------------------------------------------------------------------
//
// This is the structure a B-Tree replaces, and it exists in this file solely to
// make the B-Tree's height meaningful. "Height 8 after 1,000 keys" is not
// interesting on its own; it is interesting next to what the obvious
// alternative does with the same input.

type bstNode struct {
	key         int
	left, right *bstNode
}

type bst struct {
	root *bstNode
	n    int
}

func (b *bst) Insert(key int) bool {
	p := &b.root
	for *p != nil {
		switch {
		case key < (*p).key:
			p = &(*p).left
		case key > (*p).key:
			p = &(*p).right
		default:
			return false
		}
	}
	*p = &bstNode{key: key}
	b.n++
	return true
}

func (b *bst) Height() int {
	var h func(*bstNode) int
	h = func(n *bstNode) int {
		if n == nil {
			return 0
		}
		l, r := h(n.left), h(n.right)
		if l > r {
			return l + 1
		}
		return r + 1
	}
	return h(b.root)
}

// ---------------------------------------------------------------------------
// The experiment
// ---------------------------------------------------------------------------

// TestHeightSequentialVsRandom is the Session 6 checklist item: insert the same
// keys in ascending order and in random order, and compare the resulting trees.
//
// It is a measurement harness, not a pass/fail test beyond the invariants. Run:
//
//	go test -run TestHeightSequentialVsRandom -v ./btree/...
func TestHeightSequentialVsRandom(t *testing.T) {
	sizes := []int{1000, 10000, 100000}

	t.Log("B-Tree with t=2 (nodes hold 1-3 keys), against a naive unbalanced BST")
	t.Logf("%-9s %-12s %8s %8s %8s %9s %12s",
		"n", "order", "height", "nodes", "fill", "log2(n)", "BST height")

	for _, n := range sizes {
		for _, order := range []string{"sequential", "random"} {
			bt := New(2)
			ref := &bst{}

			keys := make([]int, n)
			for i := range keys {
				keys[i] = i
			}
			if order == "random" {
				rng := rand.New(rand.NewSource(int64(n)))
				rng.Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			}

			for _, k := range keys {
				bt.Insert(k)
				ref.Insert(k)
			}

			if err := bt.Validate(); err != nil {
				t.Fatalf("n=%d %s: %v", n, order, err)
			}
			if bt.Len() != n {
				t.Fatalf("n=%d %s: Len = %d", n, order, bt.Len())
			}

			t.Logf("%-9d %-12s %8d %8d %7.1f%% %9.1f %12d",
				n, order, bt.Height(), bt.Nodes(), 100*bt.Fill(),
				math.Log2(float64(n)), ref.Height())
		}
	}
}

// TestHeightAcrossDegrees shows why a real B-Tree is not built with t=2.
//
// Height falls as log_{t}(n): raising the branching factor lowers the tree.
// In memory this barely matters, because every node is a pointer dereference
// into RAM. On disk each level is a page read, so height is read amplification
// directly -- which is why an on-disk B-Tree sizes its nodes to fill a page and
// ends up with t in the hundreds. Session 7 makes that concrete.
func TestHeightAcrossDegrees(t *testing.T) {
	const n = 100000

	t.Logf("%d random keys", n)
	t.Logf("%-6s %10s %8s %8s %9s %14s",
		"t", "max keys", "height", "nodes", "fill", "reads/lookup")

	for _, degree := range []int{2, 4, 8, 32, 128, 256} {
		bt := New(degree)
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < n; i++ {
			bt.Insert(rng.Intn(1 << 30))
		}
		if err := bt.Validate(); err != nil {
			t.Fatalf("t=%d: %v", degree, err)
		}

		t.Logf("%-6d %10d %8d %8d %8.1f%% %14d",
			degree, 2*degree-1, bt.Height(), bt.Nodes(), 100*bt.Fill(), bt.Height())
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

const benchN = 100000

func benchTree(b *testing.B, degree int) (*BTree, []int) {
	b.Helper()
	bt := New(degree)
	rng := rand.New(rand.NewSource(99))
	keys := make([]int, 0, benchN)
	for len(keys) < benchN {
		k := rng.Intn(1 << 30)
		if bt.Insert(k) {
			keys = append(keys, k)
		}
	}
	return bt, keys
}

func BenchmarkSearch(b *testing.B) {
	for _, degree := range []int{2, 32, 128} {
		b.Run(fmt.Sprintf("t=%d", degree), func(b *testing.B) {
			bt, keys := benchTree(b, degree)
			b.ReportMetric(float64(bt.Height()), "height")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !bt.Search(keys[i%len(keys)]) {
					b.Fatal("missing key")
				}
			}
		})
	}
}

func BenchmarkInsertSequential(b *testing.B) {
	bt := New(32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bt.Insert(i)
	}
}

func BenchmarkInsertRandom(b *testing.B) {
	bt := New(32)
	rng := rand.New(rand.NewSource(5))
	keys := make([]int, b.N)
	for i := range keys {
		keys[i] = rng.Intn(1 << 30)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bt.Insert(keys[i])
	}
}
