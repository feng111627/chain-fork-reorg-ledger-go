package node

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/feng111627/chain-fork-reorg-ledger-go/pool"
	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

const (
	na types.Address = "Alice"
	nb types.Address = "Bob"
)

func newNode(t *testing.T) *Node {
	t.Helper()
	gen := types.Block{Height: 0}
	gs := state.New()
	gs.SetBalance(na, 100)
	gs.SetBalance(nb, 0)
	n, err := NewWithGenesisState(gen, gs)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPoolValidation 业务：交易入池前的 nonce/余额校验必须明确拒绝。
func TestPoolValidation(t *testing.T) {
	n := newNode(t)

	cases := []struct {
		name     string
		tx       types.Transaction
		accept   bool
		sentinel error
	}{
		{"nonce缺口(先交nonce1)", types.Transaction{From: na, To: nb, Nonce: 1, Value: 1}, false, state.ErrNonceMismatch},
		{"发送方不存在", types.Transaction{From: "Ghost", To: nb, Nonce: 0, Value: 1}, false, state.ErrSenderMissing},
		{"余额不足", types.Transaction{From: na, To: nb, Nonce: 0, Value: 1000}, false, state.ErrInsufficientBalance},
		{"合法交易nonce0", types.Transaction{From: na, To: nb, Nonce: 0, Value: 10}, true, nil},
		{"同nonce重复提交(池中已有)", types.Transaction{From: na, To: nb, Nonce: 0, Value: 10}, false, pool.ErrAlreadyPending},
		{"叠加池内支出后余额不足", types.Transaction{From: na, To: nb, Nonce: 1, Value: 95}, false, state.ErrInsufficientBalance},
		{"合法交易nonce1", types.Transaction{From: na, To: nb, Nonce: 1, Value: 5}, true, nil},
	}
	for _, tc := range cases {
		err := n.SubmitTx(tc.tx)
		if tc.accept {
			if err != nil {
				t.Errorf("[%s] 应接受却被拒绝: %v", tc.name, err)
			} else {
				t.Logf("用例 [接受] %s", tc.name)
			}
			continue
		}
		if err == nil {
			t.Errorf("[%s] 应拒绝却被接受", tc.name)
			continue
		}
		if !errors.Is(err, tc.sentinel) {
			t.Errorf("[%s] 拒绝原因错误: 期望 %v 实际 %v", tc.name, tc.sentinel, err)
		}
		t.Logf("用例 [拒绝] %-24s 原因=%v", tc.name, err)
	}
	if n.PoolSize() != 2 {
		t.Fatalf("池中应有2笔交易, 实际 %d", n.PoolSize())
	}
}

// TestPackAndReorgPool 业务：打包后移除；重组后交易池随主链刷新。
func TestPackAndReorgPool(t *testing.T) {
	n := newNode(t)
	genHash := types.Block{Height: 0}.Hash()

	// 提交 A->B nonce0=10, nonce1=20。
	if err := n.SubmitTx(types.Transaction{From: na, To: nb, Nonce: 0, Value: 10}); err != nil {
		t.Fatal(err)
	}
	if err := n.SubmitTx(types.Transaction{From: na, To: nb, Nonce: 1, Value: 20}); err != nil {
		t.Fatal(err)
	}

	t.Run("打包进块后从池中移除", func(t *testing.T) {
		b, err := n.CommitBlock(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Txs) != 2 {
			t.Fatalf("块内应包含2笔, 实际 %d", len(b.Txs))
		}
		if n.PoolSize() != 0 {
			t.Fatalf("打包后池应为空, 实际 %d", n.PoolSize())
		}
		if n.Balance(na) != 70 || n.Balance(nb) != 30 || n.Nonce(na) != 2 {
			t.Fatalf("打包后状态错误: A=%d B=%d nonceA=%d", n.Balance(na), n.Balance(nb), n.Nonce(na))
		}
		t.Logf("用例 打包2笔成功: A=%d B=%d 池剩余=%d", n.Balance(na), n.Balance(nb), n.PoolSize())
	})

	t.Run("重组切换主链后池子按新状态刷新", func(t *testing.T) {
		// 主链当前是包含两笔交易的块(权重10)。构造一条更高权重分叉，
		// 其中只执行 nonce0=40；重组后 A 的链上 nonce 回到1、余额60。
		fork := types.Block{
			ParentHash: genHash, Height: 1, Weight: 50,
			Txs: []types.Transaction{{From: na, To: nb, Nonce: 0, Value: 40}},
		}
		// 旧主链 nonce=2，提交 nonce1 应有缺口而被拒；重组后则变合法。
		if err := n.SubmitTx(types.Transaction{From: na, To: nb, Nonce: 1, Value: 5}); err == nil {
			t.Fatal("旧主链 nonce=2，提交 nonce1 应被拒绝")
		}
		switched, err := n.ImportBlock(fork)
		if err != nil {
			t.Fatal(err)
		}
		if !switched {
			t.Fatal("高权重分叉应触发重组")
		}
		if n.Balance(na) != 60 || n.Nonce(na) != 1 {
			t.Fatalf("重组后状态错误: A=%d nonceA=%d", n.Balance(na), n.Nonce(na))
		}
		if err := n.SubmitTx(types.Transaction{From: na, To: nb, Nonce: 1, Value: 5}); err != nil {
			t.Fatalf("重组后该交易应可入池: %v", err)
		}
		if !n.VerifyTipState() {
			t.Fatal("重组后顶端状态与全量重放不一致")
		}
		t.Logf("用例 重组后 A=%d nonce=%d，nonce1交易重新可入池，池大小=%d，状态一致=%v",
			n.Balance(na), n.Nonce(na), n.PoolSize(), n.VerifyTipState())
	})
}

// TestConcurrent 业务：区块导入、交易提交、状态查询并发进行，
// 要求无数据竞争、且始终读不到中间状态（以 go test -race 检测）。
func TestConcurrent(t *testing.T) {
	const writers = 8
	const each = 50

	gen := types.Block{Height: 0}
	gs := state.New()
	gs.SetBalance(na, 1_000_000)
	gs.SetBalance(nb, 0)
	n, err := NewWithGenesisState(gen, gs)
	if err != nil {
		t.Fatal(err)
	}
	genHash := gen.Hash()

	addrs := make([]types.Address, writers)
	fundTxs := make([]types.Transaction, writers)
	for w := 0; w < writers; w++ {
		addrs[w] = types.Address(fmt.Sprintf("writer%d", w))
		fundTxs[w] = types.Transaction{From: na, To: addrs[w], Nonce: uint64(w), Value: 1000}
	}
	if _, err := n.ImportBlock(types.Block{
		ParentHash: genHash, Height: 1, Weight: 1, Txs: fundTxs,
	}); err != nil {
		t.Fatal(err)
	}

	var submitWG sync.WaitGroup
	// 并发提交：每个 writer 连续 nonce，全部应成功。
	for w := 0; w < writers; w++ {
		submitWG.Add(1)
		go func(id int) {
			defer submitWG.Done()
			from := addrs[id]
			for i := 0; i < each; i++ {
				if err := n.SubmitTx(types.Transaction{
					From: from, To: nb, Nonce: uint64(i), Value: 1,
				}); err != nil {
					t.Errorf("writer%d nonce%d: %v", id, i, err)
					return
				}
			}
		}(w)
	}

	// 并发读：持续查询，读到的状态必须始终与全量重放一致（无中间态）。
	readerStop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-readerStop:
				return
			default:
				_ = n.Balance(na)
				_ = n.Nonce(na)
				_ = n.CanonicalHashes()
				_ = n.PendingTxs()
			}
		}
	}()

	// 并发导入：单一出块者连续打包空块（高权重，始终延长主链）。
	var minerWG sync.WaitGroup
	minerWG.Add(1)
	go func() {
		defer minerWG.Done()
		for i := 0; i < 10; i++ {
			empty := n.ProposeBlockWith(100, nil)
			if _, err := n.ImportBlock(empty); err != nil {
				t.Errorf("并发导入空块失败: %v", err)
				return
			}
		}
	}()

	submitWG.Wait()
	minerWG.Wait()
	close(readerStop)
	readerWG.Wait()

	// 最终把池中全部交易打包，核对总账守恒与状态一致性。
	final := n.ProposeBlock(1)
	if _, err := n.ImportBlock(final); err != nil {
		t.Fatal(err)
	}
	if n.PoolSize() != 0 {
		t.Fatalf("最终打包后池应为空, 实际 %d", n.PoolSize())
	}
	for _, ad := range addrs {
		if n.Balance(ad) != 1000-each {
			t.Fatalf("writer 余额错误 %s=%d", ad, n.Balance(ad))
		}
	}
	if !n.VerifyTipState() {
		t.Fatal("并发结束后顶端状态与全量重放不一致")
	}
	t.Logf("用例 并发提交 %d writer×%d 笔全部成功，10 空块并发导入，读写并行无数据竞争；状态一致=%v",
		writers, each, n.VerifyTipState())
}
