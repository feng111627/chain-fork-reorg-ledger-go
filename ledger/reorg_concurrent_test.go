package ledger

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConcurrentReorgPoolConsistency 覆盖并发下的最终一致性：
// 提交交易、本地出块、外部区块导入触发重组、快照读同时进行。
// 验证：
//   - -race 下无数据竞争（读绝不会看到重组做到一半的中间状态）；
//   - 任意时点快照总量守恒（不会拿失效交易出块、账不会重算/丢失）；
//   - 停止后继续出块能排空池子，最终所有提交成功的交易恰好确认一次；
//   - 最终状态与按最终主链全量重放完全一致。
func TestConcurrentReorgPoolConsistency(t *testing.T) {
	const nAcc = 6
	const txPerAcc = 20
	const reorgRounds = 12

	alloc := map[string]uint64{}
	accs := make([]string, nAcc)
	var total uint64
	for i := 0; i < nAcc; i++ {
		accs[i] = fmt.Sprintf("user%d", i)
		alloc[accs[i]] = 1000
		total += 1000
	}
	// 两个专门在分叉分支里活动的账户，与提交者账户完全不重叠。
	alloc["forker"] = 1_000_000
	total += 1_000_000
	n := NewNode(alloc)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var submitOK sync.Map // 提交成功的交易哈希 -> Tx，最终必须恰好确认一次

	// 提交者：闭环转账，提交失败就重试（重组期可能临时 nonce 不齐），
	// 成功的交易由 submitOK 记账，等待最终全部确认。
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
						submitOK.Store(tx.Hash(), tx)
						break
					}
					// 已被链上确认（可能被重组路径处理）则视为成功。
					if n.Nonce(from) > nonce {
						submitOK.Store(tx.Hash(), tx)
						break
					}
					select {
					case <-stop:
						return
					default:
					}
				}
			}
		}(acc, i)
	}

	// 出块者：持续把池子打包成块（含回流交易）。
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

	// 重组者：反复从创世构造权重足够高的竞争分支并导入，
	// 每次都把主链抢过去再被本地出块抢回来，制造持续的撤销/回流抖动。
	wg.Add(1)
	go func() {
		defer wg.Done()
		genesis := n.MainChain()[0]
		for r := 0; r < reorgRounds; r++ {
			// 动态取当前主链高度，确保分支累积权重一定能反超。
			_, curHeight := n.Head()
			weight := curHeight + 5
			// 每个分支都从创世独立重放，forker 的 nonce 都从 0 起；
			// 金额取 r+1 让各区块哈希互不相同（避免重复导入）。
			f := childOf(genesis, weight,
				Tx{From: "forker", To: accs[r%nAcc], Amount: uint64(r + 1), Nonce: 0})
			if err := n.ImportBlock(f); err != nil {
				t.Errorf("reorg import round %d: %v", r, err)
				return
			}
		}
	}()

	// 快照读者：任意时点总量必须守恒。
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
				sum += snap.Balance("forker")
				if sum != total {
					t.Errorf("total supply = %d, want %d（读到重组中间态）", sum, total)
					return
				}
			}
		}()
	}

	// 等待提交者全部完成：每个账户链上 nonce 达到 txPerAcc，
	// 且所有提交成功的交易都已确认，期间持续发生出块与重组。
	for {
		allDone := true
		submitOK.Range(func(_, v any) bool {
			tx := v.(Tx)
			if n.Nonce(tx.From) <= tx.Nonce {
				allDone = false
				return false
			}
			return true
		})
		for _, a := range accs {
			if n.Nonce(a) < txPerAcc {
				allDone = false
			}
		}
		if allDone {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()

	// 停止后继续出块，把可能残留的待打包/回流交易排空。
	for n.PoolLen() > 0 {
		if _, err := n.ProduceBlock(); err != nil {
			t.Fatalf("drain produce: %v", err)
		}
	}

	// 最终校验 1：每个提交账户 nonce 恰好 txPerAcc，余额与主链重放一致。
	finalChain := n.MainChain()
	want := replayState(t, alloc, finalChain...)
	snap := n.Snapshot()
	for _, a := range accs {
		if snap.Nonce(a) != txPerAcc {
			t.Fatalf("nonce[%s]=%d, want %d（交易丢失或卡住）", a, snap.Nonce(a), txPerAcc)
		}
		if snap.Balance(a) != want.Balance(a) {
			t.Fatalf("balance[%s]=%d, want %d", a, snap.Balance(a), want.Balance(a))
		}
	}
	// 最终校验 2：所有提交成功的交易在最终主链上恰好出现一次（不丢不重）。
	counts := make(map[[32]byte]int)
	for _, b := range finalChain {
		for _, tx := range b.Txs {
			counts[tx.Hash()]++
		}
	}
	submitOK.Range(func(k, v any) bool {
		h := k.([32]byte)
		if counts[h] != 1 {
			t.Errorf("tx %s 在最终主链出现 %d 次，want 1（不丢不重）", v.(Tx), counts[h])
		}
		return true
	})
	// forker 在每个分叉分支各发一笔，最终只可能有一笔留在主链上，
	// 其余都应作为被撤销交易回流或留在池中；池已排空，因此它们要么
	// 在主链上（恰好一次），要么因为 nonce 冲突被剔除后不再合法。
	_, h := n.Head()
	t.Logf("并发重组测试完成：%d 账户 x %d 笔全部确认，最终高度 %d，池子 %d，状态与重放一致",
		nAcc, txPerAcc, h, n.PoolLen())
}
