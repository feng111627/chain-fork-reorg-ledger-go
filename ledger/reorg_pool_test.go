package ledger

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 【业务：重组 × 交易池交叉】
// TestReorgPoolRealign 覆盖重组后池子跟随新主链重对齐的两条路径：
// 被撤销交易自动回池重排、失效交易清除且去重标记不残留。
func TestReorgPoolRealign(t *testing.T) {
	t.Run("被撤销交易自动回池并按nonce重排后可重新打包", func(t *testing.T) {
		alloc := map[string]uint64{"alice": 100, "bob": 50}
		n := NewNode(alloc)
		// 主链块 A1：alice nonce 0,1,2 上链。
		for i := uint64(0); i < 3; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		// 继续在池中排队 nonce 3,4（尚未上链）。
		for i := uint64(3); i < 5; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		// 更重空分叉把主链顶端换掉：nonce 0..2 被撤销。
		main := n.MainChain()
		g := main[0]
		b1 := childOf(g, 1)  // 空块
		b2 := childOf(b1, 3) // 加重，总权重反超
		if err := n.ImportBlock(b1); err != nil {
			t.Fatal(err)
		}
		if err := n.ImportBlock(b2); err != nil {
			t.Fatal(err)
		}
		if _, h := n.Head(); h != 2 {
			t.Fatalf("head height = %d, want 2（应已重组到空分叉）", h)
		}
		// 池中应是被撤销的 0,1,2 + 原排队的 3,4，按 nonce 排好，一笔不丢。
		pending := n.PoolPending()
		if len(pending) != 5 {
			t.Fatalf("pool len = %d, want 5（被撤销交易必须回池）", len(pending))
		}
		for i, tx := range pending {
			if tx.From != "alice" || tx.Nonce != uint64(i) {
				t.Fatalf("pending[%d] = %s, want alice nonce=%d", i, tx, i)
			}
		}
		t.Log("重组后被撤销的 nonce 0..2 自动回池，与原排队 3,4 合并为连续序列")
		// 继续出块：5 笔全部重新打包确认，不丢不重。
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0", n.PoolLen())
		}
		if got := n.Balance("carol"); got != 50 {
			t.Fatalf("carol = %d, want 50（5 笔各 10，不得丢失或重复计账）", got)
		}
		if got := n.Nonce("alice"); got != 5 {
			t.Fatalf("alice nonce = %d, want 5", got)
		}
		want := replayState(t, alloc, n.MainChain()...)
		assertStateEq(t, n.Snapshot(), want, "alice", "bob", "carol")
		t.Log("回池交易重新打包上链，最终状态与主链重放一致")
	})

	t.Run("失效交易清除且重新变合法后可再提交", func(t *testing.T) {
		alloc := map[string]uint64{"alice": 50}
		n := NewNode(alloc)
		// A1：alice nonce0 转 20 上链（链上余额 30）。
		if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 20, Nonce: 0}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		// 池中排队 nonce1 转 30（对旧主链合法）。
		stale := Tx{From: "alice", To: "carol", Amount: 30, Nonce: 1}
		if err := n.SubmitTx(stale); err != nil {
			t.Fatal(err)
		}
		// 冲突分叉：同一 nonce0 转 40（不同交易），权重更高，触发重组。
		// 新主链上 alice 余额只剩 10，池中 nonce1-转30 变得余额不足。
		main := n.MainChain()
		b1 := childOf(main[0], 5, Tx{From: "alice", To: "carol", Amount: 40, Nonce: 0})
		if err := n.ImportBlock(b1); err != nil {
			t.Fatal(err)
		}
		if got := n.Balance("alice"); got != 10 {
			t.Fatalf("alice = %d, want 10（重组后新主链状态）", got)
		}
		// 池中失效交易必须立即清掉：被撤销的 nonce0-转20 与 nonce 冲突，
		// nonce1-转30 余额不足，两者都不能留。
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0（失效交易必须清除）: %v", n.PoolLen(), n.PoolPending())
		}
		t.Log("重组后冲突交易与余额不足交易被立即清除")
		// 关键：被清掉的交易不残留去重标记——再提交同一笔，
		// 得到的应是「余额不足」而不是「重复交易」。
		if err := n.SubmitTx(stale); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("resubmit stale err = %v, want ErrInsufficientBalance（不得误报重复）", err)
		}
		// 条件满足（金额降到余额内）后重新提交必须能进池。
		if err := n.SubmitTx(Tx{From: "alice", To: "carol", Amount: 10, Nonce: 1}); err != nil {
			t.Fatalf("resubmit adjusted tx: %v", err)
		}
		if n.PoolLen() != 1 {
			t.Fatalf("pool len = %d, want 1", n.PoolLen())
		}
		t.Log("被清除交易重新提交不被误判重复，条件满足后正常入池")
	})
}

// 【业务：交易池容量上限】
// TestPoolCapacity 覆盖容量打满、明确拒绝、腾出空间后恢复、
// 以及重组回收不受容量限制（保证已确认交易不丢）。
func TestPoolCapacity(t *testing.T) {
	t.Run("打满后明确拒绝且腾出空间后恢复", func(t *testing.T) {
		n := NewNodeWithPoolCap(map[string]uint64{"alice": 1000}, 3)
		for i := uint64(0); i < 3; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: i}); err != nil {
				t.Fatalf("submit nonce=%d: %v", i, err)
			}
		}
		// 第 4 笔必须被明确拒绝，错误带容量说明且可 errors.Is 判定。
		rejected := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 3}
		err := n.SubmitTx(rejected)
		if !errors.Is(err, ErrPoolFull) {
			t.Fatalf("err = %v, want ErrPoolFull", err)
		}
		t.Logf("池满拒绝原因明确：%v", err)
		if n.PoolLen() != 3 {
			t.Fatalf("pool len = %d, want 3（拒绝不得挤掉已有交易）", n.PoolLen())
		}
		// 出块腾出空间后，刚才被拒绝的交易重新提交必须成功。
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if err := n.SubmitTx(rejected); err != nil {
			t.Fatalf("resubmit after drain: %v（被拒绝过的交易不得留下残留状态）", err)
		}
		t.Log("出块腾空间后被拒绝交易重新提交成功")
	})

	t.Run("重组回收不受容量限制保证不丢交易", func(t *testing.T) {
		n := NewNodeWithPoolCap(map[string]uint64{"alice": 1000}, 2)
		// nonce0 上链。
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 0}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		// 池中排满 nonce1, nonce2（达到容量 2）。
		for i := uint64(1); i <= 2; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		// 重组撤销 nonce0：池已满，但回收必须成功（宁可超限也不丢已确认交易）。
		main := n.MainChain()
		b1 := childOf(main[0], 1)
		b2 := childOf(b1, 3)
		if err := n.ImportBlock(b1); err != nil {
			t.Fatal(err)
		}
		if err := n.ImportBlock(b2); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 3 {
			t.Fatalf("pool len = %d, want 3（回收可超限，不得丢交易）", n.PoolLen())
		}
		t.Log("池满时重组回收的 nonce0 仍然回池，池子暂时超限但不丢交易")
		// 超限期间新提交仍被拒绝；排空后恢复。
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 3}); !errors.Is(err, ErrPoolFull) {
			t.Fatalf("err = %v, want ErrPoolFull（超限期间仍拒绝新提交）", err)
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 3}); err != nil {
			t.Fatalf("submit after drain: %v", err)
		}
		if got := n.Nonce("alice"); got != 3 {
			t.Fatalf("alice nonce = %d, want 3（0..2 全部重新确认）", got)
		}
		t.Log("排空后新交易正常入池，被撤销交易全部重新确认")
	})
}

// 【业务：并发下重组与池子的最终一致性】
// TestConcurrentReorgPoolConsistency 覆盖提交交易、导入重组分叉、出块、
// 快照查询并发进行：池子始终跟着主链走，不拿失效交易出块，读者看不到
// 重组中间状态；最终全部被接受的交易确认到账、不多不少，状态与重放一致。
func TestConcurrentReorgPoolConsistency(t *testing.T) {
	const nAcc = 6
	const txPerAcc = 40
	const nMiner = 200 // 每个分叉用一个独立矿工账户，保证分叉块互不重复
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
	// 池容量故意调小，让并发下 ErrPoolFull 拒绝 + 重试恢复的路径也被覆盖。
	n := NewNodeWithPoolCap(alloc, 64)
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
						time.Sleep(500 * time.Microsecond) // 拉长并发窗口
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

	// 重组者：反复构造能反超当前主链的空账户分叉并导入，制造持续的重组。
	var reorgs int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 每个分叉用独立矿工账户（nonce 从 0 起在其分支上自洽），
		// 块内容唯一、可反复制造重组。
		for i := 0; i < nMiner; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := miners[i]
			f1 := childOf(genesis, 1, Tx{From: m, To: m, Amount: 1, Nonce: 0})
			// 权重递增：保证压过上一轮重组留下的高累积权重，稳定制造重组。
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
		// 矿工账户用完后等待停止信号，不再制造重组。
		<-stop
	}()

	// 出块者：池非空就出块，与重组者并发争抢。
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

	// 等全部交易提交完成后停止所有角色。
	done := make(chan struct{})
	go func() {
		for {
			ok := true
			for _, a := range accs {
				// 已确认 + 在途：nonce 达到 txPerAcc 说明该账户全部提交已被链或池接收。
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

	if got := atomic.LoadInt32(&reorgs); got == 0 {
		t.Fatal("测试期间未发生重组，并发重组路径未被覆盖")
	} else {
		t.Logf("并发期间共发生 %d 次重组", got)
	}

	// 排空：不再有任何重组，被撤销回池的交易全部重新打包确认。
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

// pendingOf 返回池中某账户的待打包交易数（测试辅助）。
func pendingOf(n *Node, acc string) int {
	c := 0
	for _, tx := range n.PoolPending() {
		if tx.From == acc {
			c++
		}
	}
	return c
}
