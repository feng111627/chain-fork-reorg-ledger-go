package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestProduceBlock 覆盖本地出块：池中交易按序打包、上链后从池中移除、余额生效。
func TestProduceBlock(t *testing.T) {
	n := NewNode(map[string]uint64{"alice": 100, "bob": 50})
	for i := uint64(0); i < 3; i++ {
		if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := n.ProduceBlock()
	if err != nil {
		t.Fatal(err)
	}
	if b.Height != 1 || len(b.Txs) != 3 {
		t.Fatalf("block height=%d txs=%d, want 1/3", b.Height, len(b.Txs))
	}
	if n.PoolLen() != 0 {
		t.Fatalf("pool len = %d, want 0（打包后必须移除）", n.PoolLen())
	}
	if got := n.Balance("carol"); got != 30 {
		t.Fatalf("carol = %d, want 30", got)
	}
	if got := n.Nonce("alice"); got != 3 {
		t.Fatalf("alice nonce = %d, want 3", got)
	}
	t.Log("出块后交易从池中移除且状态生效")
}

// TestImportBlockPrunesPool 覆盖外部区块上链后交易池清理（含重组确认的交易）。
func TestImportBlockPrunesPool(t *testing.T) {
	n := NewNode(map[string]uint64{"alice": 100})
	tx := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0}
	if err := n.SubmitTx(tx); err != nil {
		t.Fatal(err)
	}
	// 外部节点把同一笔交易打包进区块并传播过来。
	main := n.MainChain()
	ext := childOf(main[len(main)-1], 1, tx)
	if err := n.ImportBlock(ext); err != nil {
		t.Fatal(err)
	}
	if n.PoolLen() != 0 {
		t.Fatalf("pool len = %d, want 0（已上链交易必须清除）", n.PoolLen())
	}
	// 已确认的交易再提交应报 nonce 错误而非重新入池。
	if err := n.SubmitTx(tx); !errors.Is(err, ErrInvalidNonce) {
		t.Fatalf("resubmit err = %v, want ErrInvalidNonce", err)
	}
	t.Log("外部区块上链后池中同交易被清除，重复提交被拒绝")
}

// TestConcurrentSubmitProduceQuery 覆盖并发：多账户并发提交、
// 后台出块、并发余额查询同时进行；用 -race 运行验证无数据竞争，
// 且任意时刻读取到的都是总量守恒的一致快照。
func TestConcurrentSubmitProduceQuery(t *testing.T) {
	const nAcc = 8
	const txPerAcc = 25
	alloc := map[string]uint64{}
	accs := make([]string, nAcc)
	var total uint64
	for i := 0; i < nAcc; i++ {
		accs[i] = fmt.Sprintf("acc%d", i)
		alloc[accs[i]] = 1000
		total += 1000
	}
	n := NewNode(alloc)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 提交者：每个账户一个 goroutine，按 nonce 顺序提交（转账给下一个账户，闭环）。
	for i, acc := range accs {
		wg.Add(1)
		go func(from string, idx int) {
			defer wg.Done()
			to := accs[(idx+1)%nAcc]
			for nonce := uint64(0); nonce < txPerAcc; nonce++ {
				if err := n.SubmitTx(Tx{From: from, To: to, Amount: 1, Nonce: nonce}); err != nil {
					t.Errorf("submit %s nonce=%d: %v", from, nonce, err)
					return
				}
			}
		}(acc, i)
	}

	// 查询者：并发读余额，校验任意快照下总量守恒（不得读到中间状态）。
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := n.Snapshot()
				var sum uint64
				for _, a := range accs {
					sum += snap.Balance(a)
				}
				if sum != total {
					t.Errorf("total supply = %d, want %d（读到不一致快照）", sum, total)
					return
				}
			}
		}()
	}

	// 出块者：持续出块直到提交者完成且池清空。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, err := n.ProduceBlock(); err != nil {
				t.Errorf("produce: %v", err)
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	// 等提交者全部完成，再让出块者排空交易池。
	done := make(chan struct{})
	go func() {
		// 等待全部交易确认：每个账户链上 nonce 达到 txPerAcc。
		for {
			ok := true
			for _, a := range accs {
				if n.Nonce(a) != txPerAcc {
					ok = false
					break
				}
			}
			if ok {
				close(done)
				return
			}
		}
	}()
	<-done
	close(stop)
	wg.Wait()

	if n.PoolLen() != 0 {
		t.Fatalf("pool len = %d, want 0", n.PoolLen())
	}
	// 最终状态必须与主链重放完全一致。
	want := replayState(t, alloc, n.MainChain()...)
	for _, a := range accs {
		if n.Balance(a) != want.Balance(a) {
			t.Fatalf("final balance[%s] = %d, want %d", a, n.Balance(a), want.Balance(a))
		}
	}
	_, h := n.Head()
	t.Logf("并发测试完成：%d 账户 x %d 笔全部确认，主链高度 %d，状态与重放一致",
		nAcc, txPerAcc, h)
}
