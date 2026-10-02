// Package pool 实现带 nonce 顺序与余额校验的交易池。
//
// 池中维护两份状态：
//   - base：主链顶端状态的独立副本（由链通过 OnNewTip 刷新）；
//   - proj：在 base 之上顺序叠加池内全部交易后的“投影状态”。
//
// 新交易用 proj 校验：因此同一发送方必须按 nonce 连续提交
// （nonce 0 未入池/未上链时，nonce 1 会因缺口被拒绝），
// 余额校验也会把池中已有的待打包支出计算在内。
package pool

import (
	"fmt"
	"sort"

	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

// 交易池自身的错误（链上状态校验错误复用 state 包中的哨兵错误）。
var (
	// ErrAlreadyPending 同一发送方的同一 nonce 交易已在池中。
	ErrAlreadyPending = poolError("transaction already pending in pool")
)

type poolError string

func (e poolError) Error() string { return string(e) }

// Is 支持包装错误的 errors.Is 比较。
func (e poolError) Is(target error) bool {
	t, ok := target.(poolError)
	return ok && t == e
}

type poolKey struct {
	from  types.Address
	nonce uint64
}

// Pool 内存交易池。非并发安全，由 node.Node 持锁访问。
type Pool struct {
	base *state.State // 主链顶端状态副本
	proj *state.State // base 叠加池内交易后的投影
	txs  map[poolKey]types.Transaction
}

// New 创建交易池，基于给定状态（的副本）进行余额/nonce 校验。
func New(base *state.State) *Pool {
	p := &Pool{txs: make(map[poolKey]types.Transaction)}
	p.OnNewTip(base)
	return p
}

// Add 将交易加入池中。
// 拒绝原因可通过 errors.Is 区分：
//   - state.ErrNonceMismatch：nonce 有缺口、重复或回退；
//   - state.ErrInsufficientBalance：叠加池内支出后余额不足；
//   - state.ErrSenderMissing：发送方账户不存在；
//   - ErrAlreadyPending：同发送方同 nonce 的交易已在池中。
func (p *Pool) Add(tx types.Transaction) error {
	key := poolKey{tx.From, tx.Nonce}
	if _, ok := p.txs[key]; ok {
		return fmt.Errorf("%w: sender %s nonce %d", ErrAlreadyPending, tx.From, tx.Nonce)
	}
	// 先基于投影状态校验（不修改状态），给出明确的拒绝原因。
	if err := p.proj.Validate(tx); err != nil {
		return err
	}
	// 校验通过才真正执行到投影并保存，保证池内交易始终构成合法序列。
	if err := p.proj.Apply(tx); err != nil {
		return err
	}
	p.txs[key] = tx
	return nil
}

// Remove 从池中移除指定交易（交易被打包进区块后调用）。
// 移除后重放剩余交易重建投影，丢弃因此变得非法的交易
// （例如被移除的是同一发送方更早的 nonce）。
func (p *Pool) Remove(tx types.Transaction) {
	delete(p.txs, poolKey{tx.From, tx.Nonce})
	p.rebuild()
}

// Pending 返回当前可打包交易的副本，顺序按发送方地址、再按 nonce 排列，
// 保证结果确定可解释。调用不修改池。
func (p *Pool) Pending() []types.Transaction {
	return p.sorted()
}

// OnNewTip 主链顶端变化（重组或正常出块）时调用：
// 以新的主链状态为基准，重放池内仍有效的交易；
// 已落账（nonce 推进）或在新主链上不再合法的交易会被清除。
func (p *Pool) OnNewTip(st *state.State) {
	p.base = st.Clone()
	p.proj = st.Clone()
	p.rebuild()
}

// rebuild 以 base 为起点，按确定顺序重放池内交易并重建投影；
// 无法通过校验的交易从池中删除。
func (p *Pool) rebuild() {
	proj := p.base.Clone()
	kept := make(map[poolKey]types.Transaction, len(p.txs))
	for _, tx := range p.sorted() {
		if err := proj.Apply(tx); err != nil {
			// 在新基准上不再合法（已上链/缺口/余额不足），丢弃。
			delete(p.txs, poolKey{tx.From, tx.Nonce})
			continue
		}
		kept[poolKey{tx.From, tx.Nonce}] = tx
	}
	p.proj = proj
}

// sorted 返回池内交易按 (From, Nonce) 排序后的副本。
func (p *Pool) sorted() []types.Transaction {
	out := make([]types.Transaction, 0, len(p.txs))
	for _, tx := range p.txs {
		out = append(out, tx)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Nonce < out[j].Nonce
	})
	return out
}

// Size 返回池中交易数量。
func (p *Pool) Size() int { return len(p.txs) }

// Content 返回池中交易的有序副本（解释/展示用）。
func (p *Pool) Content() []types.Transaction { return p.sorted() }
