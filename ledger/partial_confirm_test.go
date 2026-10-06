package ledger

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 【业务：外部区块部分确认池内交易】
// TestExternalBlockPartialConfirm 覆盖外部区块只打包某账户队列前面几笔时，
// 只有真正上链的交易出池，后面未上链且仍合法的交易必须原样保留，
// 后续出块还能照常把它们打包进去；其他账户的队列不受影响。
func TestExternalBlockPartialConfirm(t *testing.T) {
	t.Run("只确认一笔时同账户剩余合法交易保留并可继续打包", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100, "bob": 50})
		// alice 排队 nonce 0..3，bob 排队 nonce 0..1。
		for i := uint64(0); i < 4; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		for i := uint64(0); i < 2; i++ {
			if err := n.SubmitTx(Tx{From: "bob", To: "carol", Amount: 5, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		// 外部节点只把 alice 的 nonce0 打包上链（直接延长当前主链）。
		head := n.MainChain()
		ext := childOf(head[len(head)-1], 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0})
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		// 只有 nonce0 出池：alice 剩 1,2,3，bob 的 0,1 一笔都不能少。
		pending := n.PoolPending()
		if len(pending) != 5 {
			t.Fatalf("pool len = %d, want 5（部分确认不得清空未上链交易）: %v", len(pending), pending)
		}
		for i, tx := range pending[:3] {
			if tx.From != "alice" || tx.Nonce != uint64(i+1) {
				t.Fatalf("pending[%d] = %s, want alice nonce=%d", i, tx, i+1)
			}
		}
		for i, tx := range pending[3:] {
			if tx.From != "bob" || tx.Nonce != uint64(i) {
				t.Fatalf("pending[%d] = %s, want bob nonce=%d", i+3, tx, i)
			}
		}
		t.Logf("部分确认后池中保留：%v", pending)
		// 后续出块必须能照常把剩余交易全部打包。
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0（剩余交易应全部打包）", n.PoolLen())
		}
		if got := n.Nonce("alice"); got != 4 {
			t.Fatalf("alice nonce = %d, want 4", got)
		}
		if got := n.Nonce("bob"); got != 2 {
			t.Fatalf("bob nonce = %d, want 2", got)
		}
		if got := n.Balance("carol"); got != 50 {
			t.Fatalf("carol = %d, want 50（alice 4x10 + bob 2x5，不丢不重）", got)
		}
		t.Log("剩余交易照常打包上链，各账户 nonce 与到账金额正确")
	})

	t.Run("确认队列前半段后剩余交易保留", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100})
		for i := uint64(0); i < 4; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		// 外部区块一次打包 nonce0 和 nonce1（前半段）。
		head := n.MainChain()
		ext := childOf(head[len(head)-1], 1,
			Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0},
			Tx{From: "alice", To: "carol", Amount: 10, Nonce: 1},
		)
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		pending := n.PoolPending()
		if len(pending) != 2 {
			t.Fatalf("pool len = %d, want 2: %v", len(pending), pending)
		}
		for i, tx := range pending {
			if tx.Nonce != uint64(i+2) {
				t.Fatalf("pending[%d] = %s, want nonce=%d", i, tx, i+2)
			}
		}
		t.Logf("前半段确认后池中保留 nonce 2,3：%v", pending)
	})
}

// 【业务：整段前缀确认的边界】
// TestExternalBlockPrefixConfirmBoundary 覆盖一次确认把账户队列前缀整段
// （甚至全部）带上链后的边界：剩余交易按新链上状态重新判定，
// 连续且余额够的保留、失效的清除；被清除交易重新提交不得被误判重复。
func TestExternalBlockPrefixConfirmBoundary(t *testing.T) {
	t.Run("整段全部确认后池子清空且重报已确认交易按nonce拒绝", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100})
		txs := make([]Tx, 4)
		for i := uint64(0); i < 4; i++ {
			txs[i] = Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}
			if err := n.SubmitTx(txs[i]); err != nil {
				t.Fatal(err)
			}
		}
		// 外部区块把 4 笔全部打包上链。
		head := n.MainChain()
		ext := childOf(head[len(head)-1], 1, txs...)
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0（全部确认后池子必须清空）", n.PoolLen())
		}
		// 已确认的交易不得残留去重标记：再提交报 nonce 已消费，而不是重复。
		for _, tx := range txs {
			if err := n.SubmitTx(tx); !errors.Is(err, ErrInvalidNonce) {
				t.Fatalf("resubmit %s err = %v, want ErrInvalidNonce", tx, err)
			}
		}
		// 下一笔新交易正常入池。
		if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: 4}); err != nil {
			t.Fatalf("submit nonce4: %v", err)
		}
		t.Log("整段确认后池子清空，已确认交易按 nonce 拒绝，新交易正常入池")
	})

	t.Run("确认后余额不足的剩余交易被清除且重报同一笔不被误判重复", func(t *testing.T) {
		// alice 余额 25：池中 nonce0(10) + nonce1(10) + nonce2(5) 恰好合法。
		n := NewNode(map[string]uint64{"alice": 25})
		stale := Tx{From: "alice", To: "carol", Amount: 10, Nonce: 1}
		for _, tx := range []Tx{
			{From: "alice", To: "carol", Amount: 10, Nonce: 0},
			stale,
			{From: "alice", To: "carol", Amount: 5, Nonce: 2},
		} {
			if err := n.SubmitTx(tx); err != nil {
				t.Fatal(err)
			}
		}
		// 外部区块确认的是同一 nonce0 但金额 20 的冲突版本：
		// 链上余额只剩 5，池中 nonce1(10) 与 nonce2(5) 合计 15 超出，必须清除。
		head := n.MainChain()
		ext := childOf(head[len(head)-1], 1, Tx{From: "alice", To: "carol", Amount: 20, Nonce: 0})
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		if got := n.Balance("alice"); got != 5 {
			t.Fatalf("alice = %d, want 5", got)
		}
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0（余额不足的剩余交易必须清除）: %v", n.PoolLen(), n.PoolPending())
		}
		t.Log("冲突版本确认后，余额不足的剩余交易被清除")
		// 被清除的交易不残留去重标记：重报同一笔得到余额不足，而不是重复。
		if err := n.SubmitTx(stale); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("resubmit stale err = %v, want ErrInsufficientBalance（不得误报重复）", err)
		}
		// 金额降到余额内的同一 nonce 重新提交必须能进池。
		if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1}); err != nil {
			t.Fatalf("resubmit adjusted: %v", err)
		}
		if n.PoolLen() != 1 {
			t.Fatalf("pool len = %d, want 1", n.PoolLen())
		}
		t.Log("被清除交易重报不误判重复，条件满足后正常入池")
	})

	t.Run("前缀确认后剩余交易仍连续且余额够时一笔不丢", func(t *testing.T) {
		// alice 余额 40：nonce0(10) 上链后剩 30，nonce1(10)+nonce2(10)+nonce3(10) 仍合法。
		n := NewNode(map[string]uint64{"alice": 40})
		for i := uint64(0); i < 4; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		head := n.MainChain()
		ext := childOf(head[len(head)-1], 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0})
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 3 {
			t.Fatalf("pool len = %d, want 3（余额足够的剩余交易必须保留）", n.PoolLen())
		}
		// 再确认 nonce1，剩余 nonce2,3 依然保留。
		head = n.MainChain()
		ext2 := childOf(head[len(head)-1], 1, Tx{From: "alice", To: "carol", Amount: 10, Nonce: 1})
		if err := n.ImportBlock(ext2); err != nil {
			t.Fatal(err)
		}
		pending := n.PoolPending()
		if len(pending) != 2 || pending[0].Nonce != 2 || pending[1].Nonce != 3 {
			t.Fatalf("pending = %v, want nonce 2,3", pending)
		}
		t.Logf("逐笔部分确认后剩余交易始终保留：%v", pending)
	})
}

// 【业务：并发下部分确认与池子的最终一致性】
// TestConcurrentPartialConfirmConsistency 覆盖提交交易、外部区块部分确认、
// 本地出块、重组分叉、快照查询并发进行：任何时刻池子跟着当前主链走，
// 部分确认不得吞掉未上链的合法交易，读者看不到重组中间状态；
// 最终被接受的交易全部确认到账、不丢不重，状态与主链重放一致。
func TestConcurrentPartialConfirmConsistency(t *testing.T) {
	const nAcc = 6
	const txPerAcc = 30
	const nMiner = 30
	alloc := map[string]uint64{}
	accs := make([]string, nAcc)
	for i := 0; i < nAcc; i++ {
		accs[i] = fmt.Sprintf("acc%d", i)
		alloc[accs[i]] = 10_000
	}
	miners := make([]string, nMiner)
	for i := 0; i < nMiner; i++ {
		miners[i] = fmt.Sprintf("miner%d", i)
		alloc[miners[i]] = 1000
	}
	allAccs := append(append([]string{}, accs...), miners...)
	var total uint64
	for _, v := range alloc {
		total += v
	}
	n := NewNodeWithPoolCap(alloc, 128)
	genesis := n.MainChain()[0]

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 提交者：每账户一个 goroutine，按 nonce 顺序提交；
	// 池满（ErrPoolFull）是合法背压，重试即可；任何其他错误都是状态不一致。
	for i, acc := range accs {
		wg.Add(1)
		go func(from string, idx int) {
			defer wg.Done()
			to := accs[(idx+1)%nAcc]
			for nonce := uint64(0); nonce < txPerAcc; nonce++ {
				tx := Tx{From: from, To: to, Amount: 1, Nonce: nonce}
				for {
					err := n.SubmitTx(tx)
					if err == nil {
						time.Sleep(300 * time.Microsecond) // 拉长并发窗口
						break
					}
					if !errors.Is(err, ErrPoolFull) {
						t.Errorf("submit %s nonce=%d: %v（除池满外不应有别的失败）", from, nonce, err)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}
		}(acc, i)
	}

	// 外部确认者：模拟外部节点把池中某账户队列的前面几笔打包成区块
	// 传播回来（延长当前主链）。与本地出块并发争抢，制造大量部分确认。
	var extConfirms int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			pending := n.PoolPending()
			if len(pending) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			// 取第一个账户队列的前 1~2 笔（PoolPending 按账户+nonce 有序）。
			first := pending[0]
			prefix := []Tx{first}
			if len(pending) > 1 && pending[1].From == first.From {
				prefix = append(prefix, pending[1])
			}
			head := n.MainChain()
			b := childOf(head[len(head)-1], 1, prefix...)
			if err := n.ImportBlock(b); err == nil {
				atomic.AddInt32(&extConfirms, 1)
			}
			// 导入失败（与出块者争抢、父块已过期等）是正常并发竞争，直接重试。
			time.Sleep(300 * time.Microsecond)
		}
	}()

	// 重组者：反复构造能反超当前主链的矿工分叉并导入，制造持续的重组。
	var reorgs int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < nMiner; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := miners[i]
			f1 := childOf(genesis, 1, Tx{From: m, To: m, Amount: 1, Nonce: 0})
			f2 := childOf(f1, uint64((i+2)*1024), Tx{From: m, To: m, Amount: 1, Nonce: 1})
			if err := n.ImportBlock(f1); err != nil {
				continue
			}
			if err := n.ImportBlock(f2); err != nil {
				continue
			}
			for _, b := range n.MainChain() {
				if b.BlockHash == f2.BlockHash {
					atomic.AddInt32(&reorgs, 1)
					break
				}
			}
			time.Sleep(500 * time.Microsecond)
		}
		<-stop
	}()

	// 出块者：池非空就出块，与外部确认者、重组者并发争抢。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n.PoolLen() > 0 {
				if _, err := n.ProduceBlock(); err != nil {
					t.Errorf("produce: %v（池子与主链不一致才会失败）", err)
					return
				}
			} else {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// 查询者：并发读快照，任意时刻总量守恒（不得读到重组中间状态）。
	for r := 0; r < 2; r++ {
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
				for _, a := range allAccs {
					sum += snap.Balance(a)
				}
				if sum != total {
					t.Errorf("total supply = %d, want %d（读到重组中间状态）", sum, total)
					return
				}
			}
		}()
	}

	// 等全部提交者完成后停止其他角色。
	done := make(chan struct{})
	go func() {
		for {
			ok := true
			for _, a := range accs {
				// 已确认 + 在途：nonce 达到 txPerAcc 说明该账户提交全部被链或池接收。
				if n.Nonce(a)+uint64(pendingOf(n, a)) < txPerAcc {
					ok = false
					break
				}
			}
			if ok {
				close(done)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-done
	close(stop)
	wg.Wait()

	if got := atomic.LoadInt32(&extConfirms); got == 0 {
		t.Fatal("测试期间未发生外部区块部分确认，核心场景未被覆盖")
	} else {
		t.Logf("并发期间外部区块部分确认成功 %d 次", got)
	}
	if got := atomic.LoadInt32(&reorgs); got == 0 {
		t.Fatal("测试期间未发生重组，并发重组路径未被覆盖")
	} else {
		t.Logf("并发期间共发生 %d 次重组", got)
	}

	// 排空：不再有任何扰动，池中（含被撤销回池的）交易全部重新打包确认。
	for {
		empty := n.PoolLen() == 0
		all := true
		for _, a := range accs {
			if n.Nonce(a) != txPerAcc {
				all = false
				break
			}
		}
		if empty && all {
			break
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatalf("drain produce: %v", err)
		}
	}

	// 最终一致性：每账户 nonce 恰好 txPerAcc（不丢不重），
	// 总量守恒，且状态与主链全量重放逐字节一致。
	for _, a := range accs {
		if got := n.Nonce(a); got != txPerAcc {
			t.Fatalf("nonce[%s] = %d, want %d（交易丢失或重复确认）", a, got, txPerAcc)
		}
	}
	snap := n.Snapshot()
	var sum uint64
	for _, a := range allAccs {
		sum += snap.Balance(a)
	}
	if sum != total {
		t.Fatalf("final total supply = %d, want %d", sum, total)
	}
	want := replayState(t, alloc, n.MainChain()...)
	assertStateEq(t, snap, want, allAccs...)
	_, h := n.Head()
	t.Logf("最终一致：%d 账户 x %d 笔全部确认，主链高度 %d，状态与重放一致", nAcc, txPerAcc, h)
}
