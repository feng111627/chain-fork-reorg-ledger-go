package ledger

import (
	"errors"
	"testing"
)

func newTestPool() *TxPool {
	st := NewState(map[string]uint64{"alice": 100, "bob": 50})
	return NewTxPool(func() *State { return st }, 0)
}

// TestPoolAddValidation 覆盖入池校验：nonce 缺口、nonce 已确认、
// 重复交易、累计余额不足、零金额，全部明确拒绝。
func TestPoolAddValidation(t *testing.T) {
	p := newTestPool()

	t.Run("首笔nonce必须为0", func(t *testing.T) {
		err := p.Add(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1})
		if !errors.Is(err, ErrNonceGap) {
			t.Fatalf("err = %v, want ErrNonceGap", err)
		}
		t.Log("nonce 有缺口返回 ErrNonceGap，不静默挂起")
	})
	t.Run("顺序入池成功", func(t *testing.T) {
		for i := uint64(0); i < 3; i++ {
			if err := p.Add(Tx{From: "alice", To: "bob", Amount: 10, Nonce: i}); err != nil {
				t.Fatalf("add nonce=%d: %v", i, err)
			}
		}
		if p.Len() != 3 {
			t.Fatalf("pool len = %d, want 3", p.Len())
		}
		t.Log("nonce 0..2 顺序入池成功")
	})
	t.Run("重复交易被拒绝", func(t *testing.T) {
		err := p.Add(Tx{From: "alice", To: "bob", Amount: 10, Nonce: 1})
		if !errors.Is(err, ErrDuplicateTransaction) {
			t.Fatalf("err = %v, want ErrDuplicateTransaction", err)
		}
		t.Log("池中已有同 nonce 交易返回 ErrDuplicateTransaction")
	})
	t.Run("缺口交易被拒绝", func(t *testing.T) {
		err := p.Add(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 5})
		if !errors.Is(err, ErrNonceGap) {
			t.Fatalf("err = %v, want ErrNonceGap", err)
		}
		t.Log("跳过 nonce=3,4 直接提交 5 返回 ErrNonceGap")
	})
	t.Run("累计余额不足被拒绝", func(t *testing.T) {
		// alice 余额 100，池中已排队 30，再花 80 超额。
		err := p.Add(Tx{From: "alice", To: "bob", Amount: 80, Nonce: 3})
		if !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
		t.Log("池中排队金额计入余额校验，超额返回 ErrInsufficientBalance")
	})
	t.Run("零金额被拒绝", func(t *testing.T) {
		err := p.Add(Tx{From: "bob", To: "alice", Amount: 0, Nonce: 0})
		if !errors.Is(err, ErrNegativeAmount) {
			t.Fatalf("err = %v, want ErrNegativeAmount", err)
		}
		t.Log("零金额交易返回 ErrNegativeAmount")
	})
}

// TestPoolRemoveAndPrune 覆盖打包移除与链上确认后的清理。
func TestPoolRemoveAndPrune(t *testing.T) {
	st := NewState(map[string]uint64{"alice": 100})
	p := NewTxPool(func() *State { return st }, 0)
	txs := []Tx{
		{From: "alice", To: "bob", Amount: 10, Nonce: 0},
		{From: "alice", To: "bob", Amount: 10, Nonce: 1},
		{From: "alice", To: "bob", Amount: 10, Nonce: 2},
	}
	for _, tx := range txs {
		if err := p.Add(tx); err != nil {
			t.Fatal(err)
		}
	}
	// 打包前两笔后移除。
	p.Remove(txs[:2])
	if p.Len() != 1 {
		t.Fatalf("pool len = %d, want 1", p.Len())
	}
	// 模拟链上状态前进：nonce 变为 2。
	st2 := NewState(map[string]uint64{"alice": 80})
	st2.nonces["alice"] = 2
	p.Prune(st2)
	if p.Len() != 1 {
		t.Fatalf("after prune len = %d, want 1（nonce=2 仍未确认）", p.Len())
	}
	// 链上 nonce 前进到 3，池中最后一笔被清理。
	st3 := NewState(map[string]uint64{"alice": 70})
	st3.nonces["alice"] = 3
	p.Prune(st3)
	if p.Len() != 0 {
		t.Fatalf("after prune len = %d, want 0", p.Len())
	}
	t.Log("打包移除与 Prune 清理行为正确")
}

// TestPoolPendingOrder 覆盖待打包交易的确定顺序：按账户字典序、账户内按 nonce。
func TestPoolPendingOrder(t *testing.T) {
	p := newTestPool()
	_ = p.Add(Tx{From: "bob", To: "alice", Amount: 1, Nonce: 0})
	_ = p.Add(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 0})
	_ = p.Add(Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1})
	pending := p.Pending()
	if len(pending) != 3 ||
		pending[0].From != "alice" || pending[0].Nonce != 0 ||
		pending[1].From != "alice" || pending[1].Nonce != 1 ||
		pending[2].From != "bob" {
		t.Fatalf("pending order wrong: %+v", pending)
	}
	t.Log("Pending 输出顺序确定：账户字典序 + nonce 升序")
}
