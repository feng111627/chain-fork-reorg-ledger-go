package ledger

import (
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
}

// NewTxPool 创建交易池，stateFn 用于查询当前主链状态。
func NewTxPool(stateFn func() *State) *TxPool {
	return &TxPool{
		stateFn: stateFn,
		pending: make(map[string][]Tx),
		seen:    make(map[[32]byte]struct{}),
	}
}

// Add 校验并加入一笔交易；失败时返回明确错误，绝不静默丢弃。
func (p *TxPool) Add(tx Tx) error {
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
	p.pending[tx.From] = append(queued, tx)
	p.seen[h] = struct{}{}
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
