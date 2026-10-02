package ledger

// State 是账户账本状态：余额与已用 nonce。
// 所有方法都不做并发控制，并发安全由上层（Node）保证。
type State struct {
	balances map[string]uint64
	nonces   map[string]uint64 // 下一笔期望的 nonce
}

// NewState 用创世余额分配创建状态。
func NewState(genesis map[string]uint64) *State {
	s := &State{
		balances: make(map[string]uint64, len(genesis)),
		nonces:   make(map[string]uint64, len(genesis)),
	}
	for addr, bal := range genesis {
		s.balances[addr] = bal
	}
	return s
}

// Balance 查询账户余额。
func (s *State) Balance(addr string) uint64 { return s.balances[addr] }

// Nonce 查询账户下一笔期望的 nonce。
func (s *State) Nonce(addr string) uint64 { return s.nonces[addr] }

// Apply 校验并应用一笔交易。
// nonce 必须等于发送方当前期望 nonce，余额必须足够，否则返回明确错误且状态不变。
func (s *State) Apply(tx Tx) error {
	if tx.Amount == 0 {
		return ErrNegativeAmount
	}
	if tx.Nonce != s.nonces[tx.From] {
		return ErrInvalidNonce
	}
	if s.balances[tx.From] < tx.Amount {
		return ErrInsufficientBalance
	}
	s.balances[tx.From] -= tx.Amount
	s.balances[tx.To] += tx.Amount
	s.nonces[tx.From]++
	return nil
}

// Revert 逆向撤销一笔交易（调用方保证该交易此前已按序应用）。
// 用于重组时回滚旧主链分支。
func (s *State) Revert(tx Tx) {
	s.nonces[tx.From]--
	s.balances[tx.From] += tx.Amount
	s.balances[tx.To] -= tx.Amount
}

// Clone 深拷贝状态。
func (s *State) Clone() *State {
	c := &State{
		balances: make(map[string]uint64, len(s.balances)),
		nonces:   make(map[string]uint64, len(s.nonces)),
	}
	for k, v := range s.balances {
		c.balances[k] = v
	}
	for k, v := range s.nonces {
		c.nonces[k] = v
	}
	return c
}
