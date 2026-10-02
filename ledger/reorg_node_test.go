package ledger

import (
	"errors"
	"testing"
)

// findPending 在节点待打包集合里找指定账户+nonce 的交易。
func findPending(n *Node, from string, nonce uint64) (Tx, bool) {
	for _, tx := range n.Pending() {
		if tx.From == from && tx.Nonce == nonce {
			return tx, true
		}
	}
	return Tx{}, false
}

// TestNodeReorgPoolSync 覆盖重组与池子交叉的核心路径：
// 旧主链确认过、新主链没有的交易自动回流并能重新打包，最终不丢交易、
// 不重复计账；池中原待打包交易按新顶端重校验。
func TestNodeReorgPoolSync(t *testing.T) {
	alloc := map[string]uint64{"alice": 100, "bob": 100}
	n := NewNode(alloc)

	// 1. 本地出块：alice nonce=0/1 上主链。
	txA0 := Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0}
	txA1 := Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1}
	if err := n.SubmitTx(txA0); err != nil {
		t.Fatal(err)
	}
	if err := n.SubmitTx(txA1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ProduceBlock(); err != nil {
		t.Fatal(err)
	}
	if n.Nonce("alice") != 2 || n.Balance("carol") != 15 {
		t.Fatalf("本地出块后状态异常: nonce=%d carol=%d", n.Nonce("alice"), n.Balance("carol"))
	}
	t.Log("阶段1：alice nonce=0/1 在旧主链确认，carol=15")

	// 2. 导入一条从创世分叉、权重更高的新分支，触发重组。
	//    新分支：bob nonce=0；alice nonce=0 是发给 dave 的另一笔。
	main := n.MainChain()
	g := main[0]
	b1 := childOf(g, 1, Tx{From: "bob", To: "dave", Amount: 20, Nonce: 0})
	b2 := childOf(b1, 1, Tx{From: "alice", To: "dave", Amount: 30, Nonce: 0})
	b3 := childOf(b2, 3, Tx{From: "alice", To: "bob", Amount: 2, Nonce: 1})
	for _, b := range []*Block{b1, b2, b3} {
		if err := n.ImportBlock(b); err != nil {
			t.Fatalf("import fork block: %v", err)
		}
	}
	head, h := n.Head()
	if head != b3.BlockHash || h != 3 {
		t.Fatalf("reorg head height=%d, want b3@3", h)
	}
	t.Log("阶段2：主链重组到 B 分支（genesis->b1->b2->b3）")

	// 3. 池子立刻与新主链对齐：
	//    - txA0（alice nonce=0 ->carol）与新主链 nonce=0(->dave) 冲突，丢弃；
	//    - txA1（alice nonce=1 ->carol）在新主链上是另一笔(->bob)，也丢弃；
	//      两笔的去重痕迹都解除。
	if n.PoolLen() != 0 {
		t.Fatalf("pool len = %d, want 0（与新主链冲突的待打包/已撤销交易须重校验）", n.PoolLen())
	}
	// 旧交易在新主链 nonce 已被占用，重新提交得到 nonce 错误而非「重复」。
	if err := n.SubmitTx(txA0); !errors.Is(err, ErrInvalidNonce) {
		t.Fatalf("conflicting resubmit: err = %v, want ErrInvalidNonce", err)
	}
	if err := n.SubmitTx(txA1); !errors.Is(err, ErrInvalidNonce) {
		t.Fatalf("conflicting resubmit: err = %v, want ErrInvalidNonce", err)
	}
	t.Log("阶段3：与新主链冲突的交易被剔除，重提交不被旧去重痕迹挡住")

	// 校验重组后状态与全量重放一致（不重复计账）。
	want := replayState(t, alloc, b1, b2, b3)
	snap := n.Snapshot()
	for _, a := range []string{"alice", "bob", "carol", "dave"} {
		if snap.Balance(a) != want.Balance(a) {
			t.Fatalf("balance[%s]=%d, want %d", a, snap.Balance(a), want.Balance(a))
		}
	}
	if snap.Balance("carol") != 0 {
		t.Fatalf("carol=%d, want 0（旧分支收款必须撤销）", snap.Balance("carol"))
	}
	t.Log("阶段3：重组后账本与按新主链重放一致，无重复计账")
}

// TestNodeRevertedTxRepacked 覆盖被撤销交易自动回流并最终重新打包，
// 全过程不丢交易、不重复计账。
func TestNodeRevertedTxRepacked(t *testing.T) {
	alloc := map[string]uint64{"alice": 100, "bob": 100}
	n := NewNode(alloc)

	// 旧主链：alice nonce=0 ->carol。
	txA0 := Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0}
	if err := n.SubmitTx(txA0); err != nil {
		t.Fatal(err)
	}
	oldBlk, err := n.ProduceBlock()
	if err != nil {
		t.Fatal(err)
	}
	if n.PoolLen() != 0 {
		t.Fatal("确认后池子应为空")
	}

	// 新分支从创世分叉：只用 bob 的一笔，不含 alice 的交易，
	// 权重更高触发重组；alice nonce=0 被撤销，必须回流。
	main := n.MainChain()
	g := main[0]
	f1 := childOf(g, 1, Tx{From: "bob", To: "dave", Amount: 7, Nonce: 0})
	f2 := childOf(f1, 2) // 空块拉大权重
	if err := n.ImportBlock(f1); err != nil {
		t.Fatal(err)
	}
	if err := n.ImportBlock(f2); err != nil {
		t.Fatal(err)
	}
	head, _ := n.Head()
	if head != f2.BlockHash {
		t.Fatal("主链未切到权重更高的分叉")
	}
	if _, ok := findPending(n, "alice", 0); !ok {
		t.Fatalf("被撤销的 alice nonce=0 未回流，pending=%+v", n.Pending())
	}
	t.Logf("阶段1：重组后旧主链交易回流待打包，pool=%d", n.PoolLen())

	// 账本侧：carol 的收款被撤销，alice 余额与 nonce 回退。
	if n.Balance("carol") != 0 {
		t.Fatalf("carol=%d, want 0（旧转账撤销）", n.Balance("carol"))
	}
	if n.Nonce("alice") != 0 {
		t.Fatalf("alice nonce=%d, want 0", n.Nonce("alice"))
	}

	// 回流交易继续出块，应能重新打包进新区块。
	newBlk, err := n.ProduceBlock()
	if err != nil {
		t.Fatalf("回流交易出块失败: %v", err)
	}
	if len(newBlk.Txs) != 1 || newBlk.Txs[0] != txA0 {
		t.Fatalf("new block txs = %+v, want 仅回流的 txA0", newBlk.Txs)
	}
	if n.PoolLen() != 0 {
		t.Fatal("重新打包后池子应清空")
	}
	if n.Balance("carol") != 10 {
		t.Fatalf("carol=%d, want 10（回流交易重新生效一次）", n.Balance("carol"))
	}
	t.Log("阶段2：回流交易重新打包，carol 恰好收到一次（不丢、不重）")

	// 最终结构：genesis -> f1 -> f2 -> newBlk；状态与按主链全量重放一致。
	finalChain := n.MainChain()
	if len(finalChain) != 4 || finalChain[1] != f1 || finalChain[2] != f2 || finalChain[3] != newBlk {
		t.Fatalf("final chain structure wrong: %d blocks", len(finalChain))
	}
	want := replayState(t, alloc, f1, f2, newBlk)
	snap := n.Snapshot()
	for _, a := range []string{"alice", "bob", "carol", "dave"} {
		if snap.Balance(a) != want.Balance(a) || snap.Nonce(a) != want.Nonce(a) {
			t.Fatalf("%s final = bal %d nonce %d, want bal %d nonce %d",
				a, snap.Balance(a), snap.Nonce(a), want.Balance(a), want.Nonce(a))
		}
	}
	if oldBlk.BlockHash == newBlk.BlockHash {
		t.Fatal("新旧区块哈希不应相同")
	}
	t.Log("阶段3：最终状态与主链全量重放一致，交易不丢不重")
}

// TestNodePoolCapFlow 覆盖节点层容量打满、拒绝原因、腾出空间后恢复。
func TestNodePoolCapFlow(t *testing.T) {
	n := NewNodeWithCap(map[string]uint64{"alice": 1_000_000}, 3)
	for i := uint64(0); i < 3; i++ {
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: i}); err != nil {
			t.Fatal(err)
		}
	}
	err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 3})
	if !errors.Is(err, ErrPoolFull) {
		t.Fatalf("over-cap submit: err = %v, want ErrPoolFull", err)
	}
	t.Log("容量打满，超额交易被 ErrPoolFull 明确拒绝")

	// 出块腾出空间，nonce=3 重新提交成功（此前被拒绝不留痕迹）。
	if _, err := n.ProduceBlock(); err != nil {
		t.Fatal(err)
	}
	if n.PoolLen() != 0 {
		t.Fatalf("pool len = %d, want 0（打包清空 3 笔）", n.PoolLen())
	}
	for i := uint64(3); i < 6; i++ {
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: i}); err != nil {
			t.Fatalf("空间腾出后 nonce=%d 提交失败: %v", i, err)
		}
	}
	if n.PoolLen() != 3 {
		t.Fatalf("pool len = %d, want 3", n.PoolLen())
	}
	t.Log("出块腾出空间后新交易正常入池，被拒绝过的 nonce 重提也成功")
}
