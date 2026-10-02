package ledger

import (
	"fmt"
	"sort"
)

// TxPool 是交易池。入池前基于当前主链状态做校验：
//   - nonce 必须恰好等于「链上 nonce + 池中该账户已排队交易数」，
//     小了说明已上链或在池中（重复），大了说明有缺口，均明确拒绝；
//   - 该账户链上余额必须覆盖池中全部待打包交易与本次交易之和。
//
// TxPool 不做并发控制，并发安全由 Node 保证。
type TxPool struct {
	stateFn func() *State         // 返回当前主链状态快照
	pending map[string][]Tx       // 按账户分组，切片内 nonce 严格连续递增
	seen    map[[32]byte]struct{} // 交易哈希去重
	cap     int                   // 池子容量上限，0 表示用默认值
}

// DefaultPoolCap 是交易池默认容量上限。
const DefaultPoolCap = 10000

// NewTxPool 创建交易池，stateFn 用于查询当前主链状态。
func NewTxPool(stateFn func() *State) *TxPool {
	return NewTxPoolWithCap(stateFn, DefaultPoolCap)
}

// NewTxPoolWithCap 创建带显式容量上限的交易池；cap<=0 时使用默认容量。
func NewTxPoolWithCap(stateFn func() *State, cap int) *TxPool {
	if cap <= 0 {
		cap = DefaultPoolCap
	}
	return &TxPool{
		stateFn: stateFn,
		pending: make(map[string][]Tx),
		seen:    make(map[[32]byte]struct{}),
		cap:     cap,
	}
}

// Reconcile 在主链变化（出块/导入/重组）后让池子与新主链顶端对齐：
//   - confirmed 中已在新主链落账的交易被移除；
//   - reverted 中被撤销的交易自动回到候选集，重新校验合法后重新入池；
//   - 原有待打包交易按新状态（nonce/余额）重过一遍，不合法即丢弃；
//   - 与新主链冲突的交易（同一账户同一 nonce 已落账为另一笔）同样丢弃。
//
// 被丢弃或确认的交易会从去重集合中清除，条件重新满足后再次提交不会
// 因「之前提交过」而被当成重复。重组属于链上事实，撤销交易回流不受
// 容量上限限制（保证不丢交易）；新提交仍受容量约束。
func (p *TxPool) Reconcile(st *State, confirmed, reverted []Tx) {
	onChain := make(map[[32]byte]struct{}, len(confirmed))
	for _, tx := range confirmed {
		onChain[tx.Hash()] = struct{}{}
	}

	// 1. 汇总每个账户的候选交易：原待打包 + 被撤销回流。
	cand := make(map[string][]Tx)
	for acc, q := range p.pending {
		for _, tx := range q {
			if _, ok := onChain[tx.Hash()]; ok {
				continue // 已在新主链落账
			}
			cand[acc] = append(cand[acc], tx)
		}
	}
	for _, tx := range reverted {
		cand[tx.From] = append(cand[tx.From], tx)
	}

	// 2. 清空旧结构，按新状态重建。
	p.pending = make(map[string][]Tx)
	p.seen = make(map[[32]byte]struct{})

	accs := make([]string, 0, len(cand))
	for acc := range cand {
		accs = append(accs, acc)
	}
	sort.Strings(accs)
	for _, acc := range accs {
		// 同账户按 nonce 排序；同 nonce 冲突时按哈希确定保留哪一笔，
		// 保证不同节点对账结果一致。
		q := cand[acc]
		sort.Slice(q, func(i, j int) bool {
			if q[i].Nonce != q[j].Nonce {
				return q[i].Nonce < q[j].Nonce
			}
			hi, hj := q[i].Hash(), q[j].Hash()
			return compareHash(hi, hj) < 0
		})
		// nonce 去重：每个 nonce 只留排序后的第一笔。
		uniq := q[:0]
		var lastNonce uint64
		for i, tx := range q {
			if i > 0 && tx.Nonce == lastNonce {
				continue
			}
			uniq = append(uniq, tx)
			lastNonce = tx.Nonce
		}
		// 从链上 nonce 起逐笔重放：一旦 nonce 出缺口或余额不足，
		// 本笔及其后全部失效并清除（条件恢复后可重新提交）。
		base := st.Nonce(acc)
		bal := st.Balance(acc)
		next := base
		var kept []Tx
		for _, tx := range uniq {
			if tx.Amount == 0 {
				continue
			}
			if tx.Nonce < base {
				continue // 新主链上同一 nonce 已落账为另一笔，冲突丢弃
			}
			if tx.Nonce > next || bal < tx.Amount {
				break
			}
			bal -= tx.Amount
			next++
			kept = append(kept, tx)
			p.seen[tx.Hash()] = struct{}{}
		}
		if len(kept) > 0 {
			p.pending[acc] = kept
		}
	}
}

// compareHash 比较两个 32 字节哈希的字典序。
func compareHash(a, b [32]byte) int {
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Add 校验并加入一笔交易；失败时返回明确错误，绝不静默丢弃。
// 池子达到容量上限时返回 ErrPoolFull（包了具体原因），等空间腾出后可重新提交。
func (p *TxPool) Add(tx Tx) error {
	if err := p.validate(tx); err != nil {
		return err
	}
	if p.Len() >= p.cap {
		return fmt.Errorf("%w: cap=%d reached, resubmit after space is freed", ErrPoolFull, p.cap)
	}
	p.commit(tx)
	return nil
}

// validate 执行除容量以外的全部入池校验，返回哨兵错误或 nil。
func (p *TxPool) validate(tx Tx) error {
	if tx.Amount == 0 {
		return ErrNegativeAmount
	}
	h := tx.Hash()
	if _, ok := p.seen[h]; ok {
		return ErrDuplicateTransaction
	}
	st := p.stateFn()
	base := st.Nonce(tx.From)
	queued := p.pending[tx.From]
	expected := base + uint64(len(queued))
	switch {
	case tx.Nonce < base:
		// nonce 已被链上确认过。
		return ErrInvalidNonce
	case tx.Nonce < expected:
		// nonce 落在池中已排队区间，是重复交易。
		return ErrDuplicateTransaction
	case tx.Nonce > expected:
		// 中间缺了交易，明确拒绝而不是挂起。
		return ErrNonceGap
	}
	// 余额校验：链上余额需覆盖池中排队总额 + 本次金额。
	var spent uint64
	for _, q := range queued {
		spent += q.Amount
	}
	if st.Balance(tx.From) < spent+tx.Amount {
		return ErrInsufficientBalance
	}
	return nil
}

// commit 假定交易已通过校验，将其写入待打包集合与去重集合。
func (p *TxPool) commit(tx Tx) {
	p.pending[tx.From] = append(p.pending[tx.From], tx)
	p.seen[tx.Hash()] = struct{}{}
}

// Pending 按账户名字典序、账户内按 nonce 顺序返回所有待打包交易。
func (p *TxPool) Pending() []Tx {
	accs := make([]string, 0, len(p.pending))
	for a := range p.pending {
		accs = append(accs, a)
	}
	sort.Strings(accs)
	var out []Tx
	for _, a := range accs {
		out = append(out, p.pending[a]...)
	}
	return out
}

// Remove 移除已打包进区块的交易（按 From+Nonce 匹配）。
func (p *TxPool) Remove(txs []Tx) {
	for _, tx := range txs {
		q := p.pending[tx.From]
		for i, t := range q {
			if t.Nonce == tx.Nonce {
				q = append(q[:i], q[i+1:]...)
				break
			}
		}
		if len(q) == 0 {
			delete(p.pending, tx.From)
		} else {
			p.pending[tx.From] = q
		}
		delete(p.seen, tx.Hash())
	}
}

// Prune 清理 nonce 已被链上确认的交易（如重组后新主链已包含它们）。
func (p *TxPool) Prune(st *State) {
	for acc, q := range p.pending {
		base := st.Nonce(acc)
		keep := q[:0]
		for _, t := range q {
			if t.Nonce >= base {
				keep = append(keep, t)
			} else {
				delete(p.seen, t.Hash())
			}
		}
		if len(keep) == 0 {
			delete(p.pending, acc)
		} else {
			p.pending[acc] = keep
		}
	}
}

// Len 返回池中交易数量。
func (p *TxPool) Len() int {
	n := 0
	for _, q := range p.pending {
		n += len(q)
	}
	return n
}
