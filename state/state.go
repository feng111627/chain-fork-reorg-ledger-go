// Package state 维护账户余额与账户 nonce，并提供快照/回滚能力。
//
// State 本身不加锁：它只会在链节点持有全局锁时被访问，
// 因此并发安全由上层（node.Node）统一保证。
package state

import (
	"fmt"

	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

// Account 账户状态。
type Account struct {
	Balance uint64
	Nonce   uint64 // 下一笔期望交易的 nonce（已成功执行的交易数）
}

// State 账户世界状态。
type State struct {
	accounts map[types.Address]*Account
}

// New 创建空状态。
func New() *State {
	return &State{accounts: make(map[types.Address]*Account)}
}

// Clone 深拷贝状态。分叉上每一个区块都在自己的状态副本上派生，
// 因此不同分叉之间互不影响。
func (s *State) Clone() *State {
	cp := New()
	for addr, acc := range s.accounts {
		cp.accounts[addr] = &Account{Balance: acc.Balance, Nonce: acc.Nonce}
	}
	return cp
}

// GetBalance 返回账户余额（不存在的账户为 0）。
func (s *State) GetBalance(addr types.Address) uint64 {
	if acc, ok := s.accounts[addr]; ok {
		return acc.Balance
	}
	return 0
}

// GetNonce 返回账户下一笔期望的 nonce（不存在的账户为 0）。
func (s *State) GetNonce(addr types.Address) uint64 {
	if acc, ok := s.accounts[addr]; ok {
		return acc.Nonce
	}
	return 0
}

// SetBalance 创世分配/测试辅助：设置账户余额（账户不存在则创建，不改变 nonce）。
func (s *State) SetBalance(addr types.Address, balance uint64) {
	acc := s.getOrCreate(addr)
	acc.Balance = balance
}

// Accounts 返回地址 -> 账户的快照副本，便于解释与展示。
func (s *State) Accounts() map[types.Address]Account {
	out := make(map[types.Address]Account, len(s.accounts))
	for addr, acc := range s.accounts {
		out[addr] = Account{Balance: acc.Balance, Nonce: acc.Nonce}
	}
	return out
}

func (s *State) getOrCreate(addr types.Address) *Account {
	acc, ok := s.accounts[addr]
	if !ok {
		acc = &Account{}
		s.accounts[addr] = acc
	}
	return acc
}

// 可区分的校验失败原因，调用方可用 errors.Is 判别。
var (
	// ErrSenderMissing 发送账户不存在（创世未分配，无资金）。
	ErrSenderMissing = ledgerError("sender account missing")
	// ErrNonceMismatch 交易 nonce 与账户期望 nonce 不一致（缺口、重复或回退）。
	ErrNonceMismatch = ledgerError("nonce mismatch")
	// ErrInsufficientBalance 余额不足。
	ErrInsufficientBalance = ledgerError("insufficient balance")
)

type ledgerError string

func (e ledgerError) Error() string { return string(e) }

// Is 支持包装错误的 errors.Is 比较。
func (e ledgerError) Is(target error) bool {
	t, ok := target.(ledgerError)
	return ok && t == e
}

// Validate 按账户 nonce 与余额校验单笔交易，不修改状态。
//   - 发送方账户必须存在；
//   - tx.Nonce 必须严格等于账户当前 nonce（有缺口或重复都拒绝）；
//   - 发送方余额必须不少于转账金额。
func (s *State) Validate(tx types.Transaction) error {
	acc, ok := s.accounts[tx.From]
	if !ok {
		return fmt.Errorf("%w: sender %s has no account", ErrSenderMissing, tx.From)
	}
	if tx.Nonce != acc.Nonce {
		return fmt.Errorf("%w: sender %s tx nonce %d, expected %d",
			ErrNonceMismatch, tx.From, tx.Nonce, acc.Nonce)
	}
	if acc.Balance < tx.Value {
		return fmt.Errorf("%w: sender %s balance %d < value %d",
			ErrInsufficientBalance, tx.From, acc.Balance, tx.Value)
	}
	return nil
}

// Apply 校验并执行单笔交易：扣减发送方、增加接收方、推进发送方 nonce。
// 校验失败时状态保持不变。
func (s *State) Apply(tx types.Transaction) error {
	if err := s.Validate(tx); err != nil {
		return err
	}
	from := s.accounts[tx.From] // Validate 已保证存在
	to := s.getOrCreate(tx.To)
	from.Balance -= tx.Value
	to.Balance += tx.Value
	from.Nonce++
	return nil
}

// Equal 判断两个状态的全部账户余额与 nonce 是否完全一致。
// 用于重组后“顶端缓存状态”与“从创世完整重放状态”的一致性核对。
func (s *State) Equal(o *State) bool {
	if len(s.accounts) != len(o.accounts) {
		return false
	}
	for addr, a := range s.accounts {
		b, ok := o.accounts[addr]
		if !ok || *a != *b {
			return false
		}
	}
	return true
}
