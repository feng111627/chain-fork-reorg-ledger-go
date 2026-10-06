package ledger

import (
	"fmt"
	"sort"
)

// TxPool 是交易池。入池前基于当前主链状态做校验：
//   - nonce 必须恰好等于「链上 nonce + 池中该账户已排队交易数」，
//     小了说明已上链或在池中（重复），大了说明有缺口，均明确拒绝；
//   - 该账户链上余额必须覆盖池中全部待打包交易与本次交易之和；
//   - 池容量有上限，满后新提交返回 ErrPoolFull（空间腾出后可再提交）。
//
// 重组后通过 Reorg 与主链重新对齐：被撤销的交易重新入池，
// 已失效（已上链 / nonce 缺口 / 余额不足 / 同 nonce 冲突）的交易被清除，
// 被清除的交易同时移出去重标记，之后重新变合法时可再次提交。
//
// TxPool 不做并发控制，并发安全由 Node 保证。
type TxPool struct {
	stateFn  func() *State         // 返回当前主链状态快照
	capacity int                   // 容量上限；<= 0 表示不限
	pending  map[string][]Tx       // 按账户分组，切片内 nonce 严格连续递增
	seen     map[[32]byte]struct{} // 交易哈希去重
	count    int                   // 池中交易总数（避免 Len 每次遍历）
}

// NewTxPool 创建交易池，stateFn 用于查询当前主链状态。
// capacity 为容量上限（仅约束新提交；重组回收的交易不受限，保证不丢交易），
// capacity <= 0 表示不限容量。
func NewTxPool(stateFn func() *State, capacity int) *TxPool {
	return &TxPool{
		stateFn:  stateFn,
		capacity: capacity,
		pending:  make(map[string][]Tx),
		seen:     make(map[[32]byte]struct{}),
	}
}

// Add 校验并加入一笔交易；失败时返回明确错误，绝不静默丢弃。
// 池满返回 ErrPoolFull（含容量信息），被拒绝的交易未入池也未标记去重，
// 空间腾出后重新提交同一笔交易可以成功。
func (p *TxPool) Add(tx Tx) error {
	if tx.Amount == 0 {
		return ErrNegativeAmount
	}
	h := tx.Hash()
	if _, ok := p.seen[h]; ok {
		return ErrDuplicateTransaction
	}
	if p.capacity > 0 && p.count >= p.capacity {
		return fmt.Errorf("%w: %d/%d", ErrPoolFull, p.count, p.capacity)
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
	p.pending[tx.From] = append(queued, tx)
	p.seen[h] = struct{}{}
	p.count++
	return nil
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
		if _, ok := p.seen[tx.Hash()]; ok {
			delete(p.seen, tx.Hash())
			p.count--
		}
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
				p.drop(t)
			}
		}
		if len(keep) == 0 {
			delete(p.pending, acc)
		} else {
			p.pending[acc] = keep
		}
	}
}

// Reorg 在主链顶端切换（重组或新区块上链）后，把池子与新主链状态重新对齐：
//  1. 被重组撤销的交易（reverted，旧主链确认过、新主链没有）按账户 + nonce
//     顺序重新入池；与池中已有交易同 nonce 冲突时，以链上确认过的版本为准，
//     被顶掉的版本移出池并清除去重标记（之后重新变合法可再提交）；
//  2. 全量重校验：先逐笔摘掉已被链上确认的前缀（外部区块部分确认时，
//     只有真正上链的那几笔出池），剩余交易从链上 nonce 起必须连续且
//     余额可覆盖，出现缺口或余额不足的才清除，并同步清除去重标记。
//
// 重组回收不占用容量上限，保证已确认过的交易不因池满而丢失。
func (p *TxPool) Reorg(st *State, reverted []Tx) {
	// 1. 回收被撤销的交易：按账户归并，同 nonce 冲突时链上版本优先。
	if len(reverted) > 0 {
		sorted := make([]Tx, len(reverted))
		copy(sorted, reverted)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].From != sorted[j].From {
				return sorted[i].From < sorted[j].From
			}
			return sorted[i].Nonce < sorted[j].Nonce
		})
		for _, tx := range sorted {
			h := tx.Hash()
			if _, ok := p.seen[h]; ok {
				continue // 池中已有同一笔，无需回收
			}
			q := p.pending[tx.From]
			// 同 nonce 冲突：移除池中版本，链上确认过的版本顶替。
			for i, t := range q {
				if t.Nonce == tx.Nonce {
					p.drop(t)
					q = append(q[:i], q[i+1:]...)
					break
				}
			}
			// 按 nonce 升序插入。
			i := sort.Search(len(q), func(i int) bool { return q[i].Nonce > tx.Nonce })
			q = append(q, Tx{})
			copy(q[i+1:], q[i:])
			q[i] = tx
			p.pending[tx.From] = q
			p.seen[h] = struct{}{}
			p.count++
		}
	}
	// 2. 全量重校验：每个账户只保留从链上 nonce 起连续且余额可覆盖的前缀。
	for acc, q := range p.pending {
		base := st.Nonce(acc)
		bal := st.Balance(acc)
		// 先摘掉已被链上确认的前缀（nonce < base）：外部区块可能只确认了
		// 队列前面几笔，这些逐笔清除；剩余交易必须继续参与连续性校验，
		// 不能因前缀已确认而整队被清。
		rest := q
		for len(rest) > 0 && rest[0].Nonce < base {
			p.drop(rest[0])
			rest = rest[1:]
		}
		var spent uint64
		keep := rest[:0]
		for i, t := range rest {
			if t.Nonce != base+uint64(i) || spent+t.Amount > bal {
				break // 从这笔起（含）全部失效：nonce 缺口 / 余额不足
			}
			spent += t.Amount
			keep = append(keep, t)
		}
		for _, t := range rest[len(keep):] {
			p.drop(t)
		}
		if len(keep) == 0 {
			delete(p.pending, acc)
		} else {
			p.pending[acc] = keep
		}
	}
}

// drop 从去重标记中移除一笔交易并更新计数（调用方负责从 pending 中摘除）。
func (p *TxPool) drop(tx Tx) {
	if _, ok := p.seen[tx.Hash()]; ok {
		delete(p.seen, tx.Hash())
		p.count--
	}
}

// Len 返回池中交易数量。
func (p *TxPool) Len() int { return p.count }
