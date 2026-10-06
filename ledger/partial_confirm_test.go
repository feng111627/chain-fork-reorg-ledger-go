package ledger

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 【业务：外部区块部分确认 × 交易池保留】
// TestPartialConfirm 覆盖修复后的核心语义：外部区块只确认某账户队列
// 靠前的一部分交易时，只有真正上链的交易被移除，后面在新主链上仍连续、
// 余额够的待打包交易原样保留，后续出块照常打包；整段前缀确认、nonce
// 缺口、余额不足等边界只清掉真正失效的交易，且不残留去重标记。
func TestPartialConfirm(t *testing.T) {
	t.Run("只确认单笔_后面合法交易原样保留并能继续打包", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100})
		for i := uint64(0); i < 5; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: i}); err != nil {
				t.Fatalf("submit nonce=%d: %v", i, err)
			}
		}
		// 外部区块只把 nonce0 打包上链（同一笔）。
		main := n.MainChain()
		ext := childOf(main[len(main)-1], 1, Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0})
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		pending := n.PoolPending()
		if len(pending) != 4 {
			t.Fatalf("pool len = %d, want 4（部分确认不得吞掉未上链交易）: %v", len(pending), pending)
		}
		for i, tx := range pending {
			if tx.Nonce != uint64(i+1) || tx.Amount != 10 {
				t.Fatalf("pending[%d] = %s, want nonce=%d amt=10（原样保留）", i, tx, i+1)
			}
		}
		t.Log("外部区块只确认 nonce0 后，nonce1..4 原样保留，内容不变")
		// 后续出块照常把剩余 4 笔打包，nonce 连续推进。
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0", n.PoolLen())
		}
		if got := n.Nonce("alice"); got != 5 {
			t.Fatalf("alice nonce = %d, want 5", got)
		}
		if got := n.Balance("bob"); got != 50 {
			t.Fatalf("bob = %d, want 50", got)
		}
		t.Log("保留的 nonce1..4 在后续出块中照常打包，nonce 推进到 5，bob 到账 50")
	})

	t.Run("确认多笔前缀_剩余连续且余额够的保留", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100})
		// 池中排队 nonce0..5，各转 10。
		for i := uint64(0); i < 6; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: i}); err != nil {
				t.Fatalf("submit nonce=%d: %v", i, err)
			}
		}
		// 外部区块一次确认 nonce0,1,2（同三笔），链上余额剩 70、nonce=3。
		main := n.MainChain()
		ext := childOf(main[len(main)-1], 1,
			Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0},
			Tx{From: "alice", To: "bob", Amount: 10, Nonce: 1},
			Tx{From: "alice", To: "bob", Amount: 10, Nonce: 2},
		)
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		pending := n.PoolPending()
		if len(pending) != 3 {
			t.Fatalf("pool len = %d, want 3（只去上链的 0..2，保留 3..5）: %v", len(pending), pending)
		}
		for i, tx := range pending {
			if tx.Nonce != uint64(i+3) {
				t.Fatalf("pending[%d] nonce=%d, want %d", i, tx.Nonce, i+3)
			}
		}
		t.Log("外部区块确认 nonce0..2 后，未上链的 nonce3..5 仍保留且连续")
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if got := n.Nonce("alice"); got != 6 {
			t.Fatalf("alice nonce = %d, want 6", got)
		}
		if got := n.Balance("bob"); got != 60 {
			t.Fatalf("bob = %d, want 60", got)
		}
		t.Log("剩余 nonce3..5 继续出块确认，bob 累计到账 60")
	})

	t.Run("整段前缀全部确认_队列清空且可接着提交下一笔", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100})
		for i := uint64(0); i < 3; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: i}); err != nil {
				t.Fatalf("submit nonce=%d: %v", i, err)
			}
		}
		main := n.MainChain()
		ext := childOf(main[len(main)-1], 1,
			Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0},
			Tx{From: "alice", To: "bob", Amount: 10, Nonce: 1},
			Tx{From: "alice", To: "bob", Amount: 10, Nonce: 2},
		)
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		if n.PoolLen() != 0 {
			t.Fatalf("pool len = %d, want 0（整段前缀全部上链）: %v", n.PoolLen(), n.PoolPending())
		}
		if got := n.Nonce("alice"); got != 3 {
			t.Fatalf("alice nonce = %d, want 3", got)
		}
		t.Log("整段队列 nonce0..2 全部确认后池子清空")
		// 已确认的旧交易再提交应报 nonce 已确认，而不是重复。
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: 1}); !errors.Is(err, ErrInvalidNonce) {
			t.Fatalf("resubmit confirmed err = %v, want ErrInvalidNonce", err)
		}
		// 下一笔 nonce3 正常入池并能出块。
		if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: 3}); err != nil {
			t.Fatalf("submit nonce3: %v", err)
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		if got := n.Nonce("alice"); got != 4 {
			t.Fatalf("alice nonce = %d, want 4", got)
		}
		t.Log("整段确认后可接着提交 nonce3 并正常出块，不被「之前提交过」挡住")
	})

	t.Run("重组导致余额不足_只清失效后缀_清掉后不残留去重标记", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100})
		// 池中排队 nonce0..4，各转 10。
		for i := uint64(0); i < 5; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: i}); err != nil {
				t.Fatalf("submit nonce=%d: %v", i, err)
			}
		}
		// 冲突分叉：同 nonce0 转 70（不同交易），权重更高触发重组。
		// 新主链 alice 余额只剩 30、nonce 仍为 1：
		// nonce1,2,3（共 30）仍合法保留，nonce4 因余额不足被清掉。
		genesis := n.MainChain()[0]
		f1 := childOf(genesis, 5, Tx{From: "alice", To: "bob", Amount: 70, Nonce: 0})
		if err := n.ImportBlock(f1); err != nil {
			t.Fatal(err)
		}
		if got := n.Balance("alice"); got != 30 {
			t.Fatalf("alice = %d, want 30（重组后新主链状态）", got)
		}
		pending := n.PoolPending()
		if len(pending) != 3 {
			t.Fatalf("pool len = %d, want 3（nonce1..3 合法、nonce4 余额不足清除）: %v", len(pending), pending)
		}
		for i, tx := range pending {
			if tx.Nonce != uint64(i+1) {
				t.Fatalf("pending[%d] nonce=%d, want %d", i, tx.Nonce, i+1)
			}
		}
		t.Log("重组后只清掉余额不足的 nonce4，前面仍连续且够余额的 nonce1..3 保留")
		// 被清掉的 nonce4 不残留去重标记：立即重提得到「余额不足」而非「重复」。
		dropped := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 4}
		if err := n.SubmitTx(dropped); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("resubmit dropped err = %v, want ErrInsufficientBalance（不得误报重复）", err)
		}
		// 等 nonce1..3 出块后 alice 余额回到 0，仍不足；构造一笔链上能承受的
		// 替代交易验证「之前提交过」不会永久挡门：这里改用更小金额的同 nonce4。
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatal(err)
		}
		// 出块后 nonce=4、余额 0；任何金额 > 0 都余额不足。
		// 用一笔给 alice 充值的外部区块让其恢复支付能力（由 bob 转回）。
		head := n.MainChain()[len(n.MainChain())-1]
		fund := childOf(head, 5, Tx{From: "bob", To: "alice", Amount: 100, Nonce: 0})
		if err := n.ImportBlock(fund); err != nil {
			t.Fatal(err)
		}
		// 充值后 nonce4 仍未上链，重新提交当初被清掉的同一笔必须能进池。
		if err := n.SubmitTx(dropped); err != nil {
			t.Fatalf("resubmit dropped after fund: %v（被清过的交易不得永久拒入）", err)
		}
		if n.PoolLen() != 1 {
			t.Fatalf("pool len = %d, want 1", n.PoolLen())
		}
		t.Log("被余额不足清掉的 nonce4 在条件恢复后重新提交成功，未被去重标记挡死")
	})

	t.Run("同nonce冲突造成缺口_缺口起清除_同笔补齐后可再提交", func(t *testing.T) {
		// 场景一：同 nonce 冲突重组。新主链用「另一笔」nonce1（转 carol）
		// 确认到 base=2；池中旧 nonce1（转 bob）被撤销回收后在新链算已确认。
		// nonce2..4 仍与 base=2 连续、余额够，必须保留。
		st0 := NewState(map[string]uint64{"alice": 100})
		p := NewTxPool(func() *State { return st0 }, 0)
		orig := []Tx{
			{From: "alice", To: "bob", Amount: 10, Nonce: 0},
			{From: "alice", To: "bob", Amount: 10, Nonce: 1},
			{From: "alice", To: "bob", Amount: 10, Nonce: 2},
			{From: "alice", To: "bob", Amount: 10, Nonce: 3},
			{From: "alice", To: "bob", Amount: 10, Nonce: 4},
		}
		for _, tx := range orig {
			if err := p.Add(tx); err != nil {
				t.Fatal(err)
			}
		}
		// 新主链：nonce0（同笔 10）+ nonce1（另一笔转 carol 10）已确认，
		// base=2，alice 余额 80。
		st := NewState(map[string]uint64{"alice": 80, "bob": 20, "carol": 10})
		st.nonces["alice"] = 2
		reverted := []Tx{{From: "alice", To: "bob", Amount: 10, Nonce: 1}}
		p.Reorg(st, reverted)
		pending := p.Pending()
		if len(pending) != 3 {
			t.Fatalf("pool len = %d, want 3（nonce2..4 连续保留）: %v", len(pending), pending)
		}
		for i, tx := range pending {
			if tx.Nonce != uint64(i+2) {
				t.Fatalf("pending[%d] nonce=%d, want %d", i, tx.Nonce, i+2)
			}
		}
		t.Log("同 nonce 冲突重组后，冲突版本移除，后面 nonce2..4 仍连续保留")

		// 场景二：真正的 nonce 缺口（重校验的防御路径，正常 Add 不会产生）。
		// 链上 base=3（已确认 0,1,2），白盒构造队列为 [nonce2, nonce4]：
		// nonce2 已确认移除后，下一保留期望 nonce3，池中却是 nonce4 => 缺口。
		st1 := NewState(map[string]uint64{"alice": 70})
		st1.nonces["alice"] = 3
		p1 := NewTxPool(func() *State { return st1 }, 0)
		gapHead := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 2}
		gapTx := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 4}
		p1.pending["alice"] = []Tx{gapHead, gapTx}
		p1.seen[gapHead.Hash()] = struct{}{}
		p1.seen[gapTx.Hash()] = struct{}{}
		p1.count = 2
		p1.Reorg(st1, nil)
		if p1.Len() != 0 {
			t.Fatalf("pool len = %d, want 0（缺口起全部清除）: %v", p1.Len(), p1.Pending())
		}
		t.Log("链上 base=3 而池中下一交易是 nonce4（缺 nonce3）时，nonce4 作缺口清除")
		// 去重标记不残留：先补 nonce3，再提交被清掉的同一笔 nonce4 必须成功。
		if err := p1.Add(Tx{From: "alice", To: "bob", Amount: 10, Nonce: 3}); err != nil {
			t.Fatalf("add nonce3: %v", err)
		}
		if err := p1.Add(gapTx); err != nil {
			t.Fatalf("resubmit cleared nonce4: %v（不得因之前提交过被当重复）", err)
		}
		if p1.Len() != 2 {
			t.Fatalf("pool len = %d, want 2", p1.Len())
		}
		t.Log("缺口清掉的 nonce4 在补齐 nonce3 后用同一笔重新提交成功")
	})

	t.Run("部分确认只影响对应账户_其他账户交易不受牵连", func(t *testing.T) {
		n := NewNode(map[string]uint64{"alice": 100, "carol": 100})
		for i := uint64(0); i < 3; i++ {
			if err := n.SubmitTx(Tx{From: "alice", To: "bob", Amount: 10, Nonce: i}); err != nil {
				t.Fatal(err)
			}
			if err := n.SubmitTx(Tx{From: "carol", To: "bob", Amount: 5, Nonce: i}); err != nil {
				t.Fatal(err)
			}
		}
		// 外部区块只确认 alice 的 nonce0。
		main := n.MainChain()
		ext := childOf(main[len(main)-1], 1, Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0})
		if err := n.ImportBlock(ext); err != nil {
			t.Fatal(err)
		}
		pending := n.PoolPending()
		// alice 剩 nonce1,2；carol 的 nonce0..2 一笔不动。
		if len(pending) != 5 {
			t.Fatalf("pool len = %d, want 5: %v", len(pending), pending)
		}
		var alice, carol []uint64
		for _, tx := range pending {
			switch tx.From {
			case "alice":
				alice = append(alice, tx.Nonce)
			case "carol":
				carol = append(carol, tx.Nonce)
			}
		}
		if len(alice) != 2 || alice[0] != 1 || alice[1] != 2 {
			t.Fatalf("alice pending nonces = %v, want [1 2]", alice)
		}
		if len(carol) != 3 || carol[0] != 0 || carol[1] != 1 || carol[2] != 2 {
			t.Fatalf("carol pending nonces = %v, want [0 1 2]（不受牵连）", carol)
		}
		t.Log("alice 部分确认只清理 alice 已上链那笔，carol 队列完整保留")
	})
}

// 【业务：并发部分确认 × 池子跟随主链的最终一致性】
// TestConcurrentPartialConfirmConsistency 覆盖提交交易、外部区块「只确认
// 前缀」导入、本地出块、重组、快照查询并发进行：
//   - 任何时刻池子都跟随当前主链：每账户待打包交易必须恰好是链上 nonce
//     起的连续、余额可覆盖后缀，既不能因部分确认吞掉合法后缀，也不能
//     残留失效交易去出块；
//   - 读者读到的状态总量守恒（看不到重组做了一半的中间状态）；
//   - 最终全部交易不丢不重确认到账，状态与主链全量重放一致。
func TestConcurrentPartialConfirmConsistency(t *testing.T) {
	const nAcc = 4
	const txPerAcc = 30
	const nMiner = 200 // 每个外部块用独立矿工账户，保证块内容唯一、可反复制造分叉
	alloc := map[string]uint64{}
	accs := make([]string, nAcc)
	var total uint64
	for i := 0; i < nAcc; i++ {
		accs[i] = fmt.Sprintf("user%d", i)
		alloc[accs[i]] = 100_000
		total += 100_000
	}
	miners := make([]string, nMiner)
	for i := 0; i < nMiner; i++ {
		miners[i] = fmt.Sprintf("miner%d", i)
		alloc[miners[i]] = 100
		total += 100
	}
	// 容量故意调小，让并发下池满背压 + 重试恢复路径也被覆盖。
	n := NewNodeWithPoolCap(alloc, 40)
	genesis := n.MainChain()[0]

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 提交者：每账户一个 goroutine，按 nonce 顺序提交；池满可重试，
	// 其余错误都意味着池子与主链不一致。
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

	// 外部部分确认者：随机挑一个账户，只把其队列「靠前的 1~2 笔」连同
	// 一笔唯一矿工交易打包进外部区块导入，稳定制造「只确认前缀」。
	// 与出块者并发抢同一前缀时，输的一方会收到 nonce 错误，属正常，跳过即可。
	var partialConfirms int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		rnd := randNew()
		for i := 0; i < nMiner; i++ {
			select {
			case <-stop:
				return
			default:
			}
			acc := accs[int(rnd.Int63())%nAcc]
			pending := pendingTxsOf(n, acc)
			k := 1 + int(rnd.Int63()%2) // 只取前缀 1 或 2 笔
			if len(pending) < k {
				k = len(pending)
			}
			if k == 0 {
				time.Sleep(200 * time.Microsecond)
				continue
			}
			txs := append([]Tx{{From: miners[i], To: miners[i], Amount: 1, Nonce: 0}}, pending[:k]...)
			headHash, _ := n.Head()
			head := blockByHash(n, headHash)
			ext := childOf(head, 1, txs...)
			if err := n.ImportBlock(ext); err == nil {
				atomic.AddInt32(&partialConfirms, 1)
			}
			time.Sleep(200 * time.Microsecond)
		}
		<-stop
	}()

	// 出块者：池非空就出块，与部分确认者并发争抢。
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
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()

	// 重组者：周期性导入能反超主链的加权空分叉，让「部分确认的块」也
	// 可能被撤销再回放，覆盖重组 × 部分确认交叉路径。
	var reorgs int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := miners[(i*7+1)%nMiner]
			f1 := childOf(genesis, 1, Tx{From: m, To: m, Amount: 1, Nonce: 0})
			f2 := childOf(f1, uint64((i+2)*512), Tx{From: m, To: m, Amount: 1, Nonce: 1})
			if n.ImportBlock(f1) == nil && n.ImportBlock(f2) == nil {
				hh, _ := n.Head()
				if hh == f2.BlockHash {
					atomic.AddInt32(&reorgs, 1)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		<-stop
	}()

	// 校验者：并发读快照与池子，校验两条一致性不变量：
	//  1. 池中每账户交易恰好是链上 nonce 起的连续后缀（误删合法后缀 /
	//     残留失效交易都会被抓到）；
	//  2. 任意快照下总量守恒（看不到重组中间状态）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 关键：在同一读锁内同时取链状态与池子，保证二者来自同一个
			// 已完成操作之后的时点（否则两个独立读锁之间可能夹着一次写入）。
			n.mu.RLock()
			snap := n.chain.State()
			pendingNow := n.pool.Pending()
			n.mu.RUnlock()
			var sum uint64
			for a := range alloc {
				sum += snap.Balance(a)
			}
			if sum != total {
				t.Errorf("total supply = %d, want %d（读到重组中间状态）", sum, total)
				return
			}
			byAcc := map[string][]Tx{}
			for _, tx := range pendingNow {
				byAcc[tx.From] = append(byAcc[tx.From], tx)
			}
			for _, a := range accs {
				q := byAcc[a]
				base := snap.Nonce(a)
				bal := snap.Balance(a)
				var spent uint64
				for i, tx := range q {
					if tx.Nonce != base+uint64(i) {
						t.Errorf("pool nonce 不连续：%s 链上nonce=%d 池内=%v（部分确认误删或残留）",
							a, base, noncesOf(q))
						return
					}
					spent += tx.Amount
					if spent > bal {
						t.Errorf("pool 余额不可覆盖：%s bal=%d spent=%d（拿失效交易出块风险）", a, bal, spent)
						return
					}
				}
			}
		}
	}()

	// 等提交者全部完成（已确认 + 在池 == txPerAcc）。
	done := make(chan struct{})
	go func() {
		for {
			ok := true
			for _, a := range accs {
				if n.Nonce(a)+uint64(len(pendingTxsOf(n, a))) < txPerAcc {
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

	if got := atomic.LoadInt32(&partialConfirms); got == 0 {
		t.Fatal("并发期间未发生任何外部部分确认，目标路径未被覆盖")
	}
	t.Logf("并发期间共发生 %d 次外部部分确认、%d 次重组",
		atomic.LoadInt32(&partialConfirms), atomic.LoadInt32(&reorgs))

	// 排空：不再有导入/重组，剩余合法后缀全部出块确认。
	for i := 0; i < 10000; i++ {
		if n.PoolLen() == 0 {
			break
		}
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatalf("drain produce: %v", err)
		}
	}
	for _, a := range accs {
		if got := n.Nonce(a); got != txPerAcc {
			t.Fatalf("nonce[%s] = %d, want %d（交易被部分确认吞掉或重复确认）", a, got, txPerAcc)
		}
	}
	if n.PoolLen() != 0 {
		t.Fatalf("pool len = %d, want 0", n.PoolLen())
	}
	snap := n.Snapshot()
	var sum uint64
	for a := range alloc {
		sum += snap.Balance(a)
	}
	if sum != total {
		t.Fatalf("final total supply = %d, want %d", sum, total)
	}
	want := replayState(t, alloc, n.MainChain()...)
	assertStateEq(t, snap, want, accs...)
	_, h := n.Head()
	t.Logf("最终一致：%d 账户 x %d 笔全部确认，主链高度 %d，状态与重放一致", nAcc, txPerAcc, h)
}

// pendingTxsOf 返回池中某账户的待打包交易（测试辅助）。
func pendingTxsOf(n *Node, acc string) []Tx {
	var out []Tx
	for _, tx := range n.PoolPending() {
		if tx.From == acc {
			out = append(out, tx)
		}
	}
	return out
}

func noncesOf(q []Tx) []uint64 {
	out := make([]uint64, len(q))
	for i, tx := range q {
		out[i] = tx.Nonce
	}
	return out
}

func blockByHash(n *Node, hash [32]byte) *Block {
	for _, b := range n.MainChain() {
		if b.BlockHash == hash {
			return b
		}
	}
	return n.MainChain()[0]
}

func randNew() *rand.Rand { return rand.New(rand.NewSource(time.Now().UnixNano())) }
