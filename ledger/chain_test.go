package ledger

import (
	"bytes"
	"errors"
	"testing"
)

func testAlloc() map[string]uint64 {
	return map[string]uint64{"alice": 100, "bob": 50}
}

func testGenesis() *Block {
	g := &Block{Height: 0, Weight: 1}
	g.Seal()
	return g
}

// childOf 构造 parent 的子区块并密封。
func childOf(parent *Block, weight uint64, txs ...Tx) *Block {
	b := &Block{
		Height: parent.Height + 1,
		Parent: parent.BlockHash,
		Txs:    txs,
		Weight: weight,
	}
	b.Seal()
	return b
}

// replayState 从创世分配按区块顺序全量重放，作为状态一致性的基准。
func replayState(t *testing.T, alloc map[string]uint64, blocks ...*Block) *State {
	t.Helper()
	st := NewState(alloc)
	for _, b := range blocks {
		for _, tx := range b.Txs {
			if err := st.Apply(tx); err != nil {
				t.Fatalf("replay apply failed: %v", err)
			}
		}
	}
	return st
}

func assertStateEq(t *testing.T, got *State, want *State, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		if got.Balance(a) != want.Balance(a) {
			t.Errorf("balance[%s] = %d, want %d", a, got.Balance(a), want.Balance(a))
		}
		if got.Nonce(a) != want.Nonce(a) {
			t.Errorf("nonce[%s] = %d, want %d", a, got.Nonce(a), want.Nonce(a))
		}
	}
}

// TestImportValidation 覆盖区块导入校验：篡改、重复、父缺失、高度错乱、
// 块内非法交易，各类原因必须可区分。
func TestImportValidation(t *testing.T) {
	g := testGenesis()
	c := NewChain(g, testAlloc())
	a1 := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0})

	t.Run("篡改内容被拒绝", func(t *testing.T) {
		bad := *a1
		bad.Txs = []Tx{{From: "alice", To: "carol", Amount: 99, Nonce: 0}} // 改内容不重算哈希
		if err := c.Import(&bad); !errors.Is(err, ErrTamperedHash) {
			t.Fatalf("err = %v, want ErrTamperedHash", err)
		}
		t.Log("篡改区块返回 ErrTamperedHash，链状态未受影响")
	})
	t.Run("父区块缺失被拒绝", func(t *testing.T) {
		orphan := childOf(a1, 1, Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1})
		if err := c.Import(orphan); !errors.Is(err, ErrUnknownParent) {
			t.Fatalf("err = %v, want ErrUnknownParent", err)
		}
		t.Log("父缺失的孤儿块返回 ErrUnknownParent")
	})
	t.Run("高度错乱被拒绝", func(t *testing.T) {
		bad := &Block{Height: 7, Parent: g.BlockHash, Weight: 1}
		bad.Seal()
		if err := c.Import(bad); !errors.Is(err, ErrBadHeight) {
			t.Fatalf("err = %v, want ErrBadHeight", err)
		}
		t.Log("高度错乱返回 ErrBadHeight")
	})
	t.Run("块内余额不足被拒绝", func(t *testing.T) {
		bad := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 1000, Nonce: 0})
		if err := c.Import(bad); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
		t.Log("块内交易余额不足返回 ErrInsufficientBalance")
	})
	t.Run("块内nonce错乱被拒绝", func(t *testing.T) {
		bad := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 1, Nonce: 5})
		if err := c.Import(bad); !errors.Is(err, ErrInvalidNonce) {
			t.Fatalf("err = %v, want ErrInvalidNonce", err)
		}
		t.Log("块内交易 nonce 错乱返回 ErrInvalidNonce")
	})
	t.Run("正常导入后重复导入被拒绝", func(t *testing.T) {
		if err := c.Import(a1); err != nil {
			t.Fatalf("first import failed: %v", err)
		}
		if err := c.Import(a1); !errors.Is(err, ErrDuplicateBlock) {
			t.Fatalf("err = %v, want ErrDuplicateBlock", err)
		}
		if got := c.State().Balance("carol"); got != 10 {
			t.Fatalf("carol balance = %d, want 10（重复导入不得重复生效）", got)
		}
		t.Log("重复导入返回 ErrDuplicateBlock，且交易未重复生效")
	})
}

// TestOutOfOrderImport 覆盖乱序到达：子块先到被拒，父块补齐后子块可导入，
// 期间已有链状态不被打乱。
func TestOutOfOrderImport(t *testing.T) {
	g := testGenesis()
	c := NewChain(g, testAlloc())
	a1 := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0})
	a2 := childOf(a1, 1, Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1})

	if err := c.Import(a2); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("out-of-order child: err = %v, want ErrUnknownParent", err)
	}
	if h := c.Head(); h.Height != 0 {
		t.Fatalf("head height = %d, want 0（乱序块不得改变链状态）", h.Height)
	}
	if err := c.Import(a1); err != nil {
		t.Fatalf("import parent: %v", err)
	}
	if err := c.Import(a2); err != nil {
		t.Fatalf("import child after parent: %v", err)
	}
	want := replayState(t, testAlloc(), a1, a2)
	assertStateEq(t, c.State(), want, "alice", "carol")
	t.Log("乱序子块先被拒、父块补齐后正常衔接，状态与顺序重放一致")
}

// TestReorg 覆盖链重组：更高权重分支出现时切换主链，
// 旧分支撤销、新分支按序生效，结果与直接重放完全一致。
func TestReorg(t *testing.T) {
	alloc := testAlloc()
	g := testGenesis()
	c := NewChain(g, alloc)

	// 主分支 A：两笔 alice->carol。
	a1 := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0})
	a2 := childOf(a1, 2, Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1})
	// 侧分支 B：从创世分叉；前两块权重不足，第三块加重后总权重反超。
	b1 := childOf(g, 1, Tx{From: "bob", To: "dave", Amount: 20, Nonce: 0})
	b2 := childOf(b1, 1, Tx{From: "alice", To: "dave", Amount: 30, Nonce: 0})
	b3 := childOf(b2, 2, Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1})

	for _, b := range []*Block{a1, a2, b1, b2} {
		if err := c.Import(b); err != nil {
			t.Fatalf("import %x: %v", b.BlockHash[:6], err)
		}
	}
	if h := c.Head(); h.BlockHash != a2.BlockHash {
		t.Fatalf("head = %x, want A 分支顶端（B 权重尚不足）", h.BlockHash[:6])
	}
	if got := c.State().Balance("carol"); got != 15 {
		t.Fatalf("carol = %d, want 15（A 分支仍为主链）", got)
	}
	t.Log("B 分支权重追平前主链保持 A 分支")

	if err := c.Import(b3); err != nil {
		t.Fatalf("import b3: %v", err)
	}
	if h := c.Head(); h.BlockHash != b3.BlockHash {
		t.Fatalf("head = %x, want B 分支顶端（重组后）", h.BlockHash[:6])
	}
	// 重组后状态必须与「从创世按 B 分支顺序重放」完全一致：
	// carol 在 B 分支没收到钱（旧分支改动被撤销，不能重复计算或丢失）。
	want := replayState(t, alloc, b1, b2, b3)
	assertStateEq(t, c.State(), want, "alice", "bob", "carol", "dave")
	if got := c.State().Balance("carol"); got != 0 {
		t.Fatalf("carol = %d, want 0（旧分支转账必须被撤销）", got)
	}
	t.Logf("重组完成：head=%x height=%d，状态与按主链重放一致", b3.BlockHash[:6], b3.Height)

	// 主链结构检查：genesis -> b1 -> b2 -> b3。
	main := c.MainChain()
	if len(main) != 4 || main[1] != b1 || main[2] != b2 || main[3] != b3 {
		t.Fatalf("main chain structure wrong: got %d blocks", len(main))
	}
}

// TestForkChoiceTieBreak 覆盖权重相同分叉：按区块哈希字典序小者胜出，规则确定可解释。
func TestForkChoiceTieBreak(t *testing.T) {
	g := testGenesis()
	c := NewChain(g, testAlloc())
	x := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 1, Nonce: 0})
	y := childOf(g, 1, Tx{From: "bob", To: "dave", Amount: 1, Nonce: 0})

	winner, loser := x, y
	if bytes.Compare(y.BlockHash[:], x.BlockHash[:]) < 0 {
		winner, loser = y, x
	}
	// 先导入输家再导入赢家，最终主链必须切到赢家。
	if err := c.Import(loser); err != nil {
		t.Fatal(err)
	}
	if err := c.Import(winner); err != nil {
		t.Fatal(err)
	}
	if h := c.Head(); h.BlockHash != winner.BlockHash {
		t.Fatalf("head = %x, want 哈希更小者 %x", h.BlockHash[:6], winner.BlockHash[:6])
	}
	t.Logf("权重相同时哈希字典序小者 %x 胜出", winner.BlockHash[:6])
}

// TestReorgThenExtend 覆盖重组后继续出块：新块接在新主链上，nonce 与余额连续。
func TestReorgThenExtend(t *testing.T) {
	alloc := testAlloc()
	g := testGenesis()
	c := NewChain(g, alloc)

	a1 := childOf(g, 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0})
	b1 := childOf(g, 1, Tx{From: "bob", To: "dave", Amount: 20, Nonce: 0})
	b2 := childOf(b1, 1, Tx{From: "alice", To: "dave", Amount: 30, Nonce: 0})

	for _, b := range []*Block{a1, b1, b2} {
		if err := c.Import(b); err != nil {
			t.Fatal(err)
		}
	}
	// 重组到 B 后，alice 的链上 nonce 被 b2 用掉一个，应为 1。
	if n := c.State().Nonce("alice"); n != 1 {
		t.Fatalf("alice nonce = %d, want 1", n)
	}
	// 在新主链上续一块：alice 的下一笔必须用 nonce=1。
	b3 := childOf(b2, 1, Tx{From: "alice", To: "carol", Amount: 7, Nonce: 1})
	if err := c.Import(b3); err != nil {
		t.Fatalf("extend after reorg: %v", err)
	}
	want := replayState(t, alloc, b1, b2, b3)
	assertStateEq(t, c.State(), want, "alice", "bob", "carol", "dave")
	t.Log("重组后续块正常，nonce 序列连续、状态与重放一致")
}
