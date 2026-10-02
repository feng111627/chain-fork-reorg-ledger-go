package ledger

import (
	"errors"
	"testing"
)

// TestPoolReconcileReverted 覆盖重组撤销交易回流：原主链确认过、新主链
// 没有的交易必须自动回到待打包集合，并按账户与 nonce 顺序排好。
func TestPoolReconcileReverted(t *testing.T) {
	t.Run("被撤销交易按nonce顺序回流", func(t *testing.T) {
		st := NewState(map[string]uint64{"alice": 100})
		p := NewTxPool(func() *State { return st })
		// 旧主链上 alice 的 nonce 0/1 已确认，重组后新主链没有这两笔，
		// 链上 nonce 退回 0、余额退回 100。
		old0 := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0}
		old1 := Tx{From: "alice", To: "bob", Amount: 5, Nonce: 1}
		p.Reconcile(st, nil, []Tx{old1, old0})
		if p.Len() != 2 {
			t.Fatalf("pool len = %d, want 2（撤销交易必须回流）", p.Len())
		}
		got := p.Pending()
		if got[0].Nonce != 0 || got[1].Nonce != 1 {
			t.Fatalf("pending order wrong: nonce %d,%d", got[0].Nonce, got[1].Nonce)
		}
		t.Log("两笔被撤销交易回流，且按 nonce 0->1 排好")
	})

	t.Run("回流交易与池中待打包交易同nonce冲突只留一笔", func(t *testing.T) {
		// 新主链 nonce=1：池中已有一笔 nonce=1，被撤销的旧主链也有一笔
		// nonce=1，二者冲突，按哈希确定性保留一笔。
		st := NewState(map[string]uint64{"alice": 100})
		st.nonces["alice"] = 1
		p := NewTxPool(func() *State { return st })
		inPool := Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1}
		old1 := Tx{From: "alice", To: "bob", Amount: 9, Nonce: 1}
		p.commit(inPool)
		p.Reconcile(st, nil, []Tx{old1})
		if p.Len() != 1 {
			t.Fatalf("pool len = %d, want 1（同 nonce 冲突只能留一笔）", p.Len())
		}
		t.Logf("回流与池中同 nonce 冲突，确定性保留 %s", p.Pending()[0])
	})
}

// TestPoolReconcileInvalidation 覆盖重组后待打包交易按新顶端重过一遍：
// 已落账、nonce 出缺口、余额不足、与新主链冲突的都被清掉，
// 且清掉后重新提交不再被当成重复。
func TestPoolReconcileInvalidation(t *testing.T) {
	t.Run("已在新主链落账的被清掉", func(t *testing.T) {
		st := NewState(map[string]uint64{"alice": 90})
		st.nonces["alice"] = 1
		p := NewTxPool(func() *State { return st })
		tx0 := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0}
		tx1 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1}
		p.commit(tx0)
		p.commit(tx1)
		// 新主链确认了 tx0（内容相同），链上 nonce=1。
		p.Reconcile(st, []Tx{tx0}, nil)
		if p.Len() != 1 || p.Pending()[0].Nonce != 1 {
			t.Fatalf("pool len = %d, want 仅 nonce=1", p.Len())
		}
		t.Log("已落账交易从待打包集合清除，后续交易保留")
	})

	t.Run("nonce出缺口的后续交易全部清掉且可重新提交", func(t *testing.T) {
		st := NewState(map[string]uint64{"alice": 100})
		p := NewTxPool(func() *State { return st })
		tx0 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 0}
		tx1 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1}
		tx2 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 2}
		p.commit(tx0)
		p.commit(tx1)
		p.commit(tx2)
		// 新主链直接确认到 nonce 2：tx0/tx1 失效，tx2 保留。
		st.nonces["alice"] = 2
		p.Reconcile(st, nil, nil)
		if p.Len() != 1 || p.Pending()[0].Nonce != 2 {
			t.Fatalf("pool len = %d, want 仅 nonce=2", p.Len())
		}
		// 被清掉的 tx0 再提交应得到 nonce 已过期错误，而不是重复。
		if err := p.Add(tx0); !errors.Is(err, ErrInvalidNonce) {
			t.Fatalf("resubmit confirmed tx: err = %v, want ErrInvalidNonce", err)
		}
		t.Log("缺口前的失效交易被清，重提交报 nonce 错误而非重复")
	})

	t.Run("余额不足的交易被清掉，到账后重新提交成功", func(t *testing.T) {
		st := NewState(map[string]uint64{"alice": 10})
		p := NewTxPool(func() *State { return st })
		tx0 := Tx{From: "alice", To: "bob", Amount: 5, Nonce: 0}
		tx1 := Tx{From: "alice", To: "bob", Amount: 50, Nonce: 1}
		p.commit(tx0)
		p.commit(tx1)
		// 重组后 alice 余额只剩 10：tx0 能覆盖，tx1 不能 -> tx1 被清。
		p.Reconcile(st, nil, nil)
		if p.Len() != 1 || p.Pending()[0].Nonce != 0 {
			t.Fatalf("pool len = %d, want 仅 nonce=0", p.Len())
		}
		// 去重痕迹已清除：仍余额不足时报余额错误（不是重复）。
		if err := p.Add(tx1); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("resubmit underfunded tx: err = %v, want ErrInsufficientBalance", err)
		}
		st.balances["alice"] = 100
		if err := p.Add(tx1); err != nil {
			t.Fatalf("resubmit after refill: %v（不应再报重复）", err)
		}
		if p.Len() != 2 {
			t.Fatalf("pool len = %d, want 2", p.Len())
		}
		t.Log("余额不足剔除后去重痕迹解除，到账重新提交成功")
	})

	t.Run("与新主链同nonce冲突的交易被清掉", func(t *testing.T) {
		st := NewState(map[string]uint64{"alice": 100})
		st.nonces["alice"] = 1
		p := NewTxPool(func() *State { return st })
		poolTx := Tx{From: "alice", To: "bob", Amount: 10, Nonce: 0}
		chainTx := Tx{From: "alice", To: "carol", Amount: 3, Nonce: 0}
		p.commit(poolTx)
		// 新主链把 nonce 0 确认成了另一笔 chainTx。
		p.Reconcile(st, []Tx{chainTx}, nil)
		if p.Len() != 0 {
			t.Fatalf("pool len = %d, want 0（冲突交易必须清除）", p.Len())
		}
		// 同一笔 poolTx 重新提交：nonce 已被占用，报 ErrInvalidNonce 而非重复。
		if err := p.Add(poolTx); !errors.Is(err, ErrInvalidNonce) {
			t.Fatalf("err = %v, want ErrInvalidNonce", err)
		}
		t.Log("同 nonce 冲突交易清除，重提交不被旧去重痕迹挡住")
	})
}

// TestPoolCap 覆盖容量上限：打满后明确拒绝，腾出空间后可再进，
// 被拒绝过的交易条件满足后重提成功。
func TestPoolCap(t *testing.T) {
	st := NewState(map[string]uint64{"alice": 1_000_000})
	p := NewTxPoolWithCap(func() *State { return st }, 2)

	tx0 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 0}
	tx1 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1}
	tx2 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 2}
	if err := p.Add(tx0); err != nil {
		t.Fatal(err)
	}
	if err := p.Add(tx1); err != nil {
		t.Fatal(err)
	}
	if err := p.Add(tx2); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("full pool err = %v, want ErrPoolFull", err)
	}
	t.Log("池子打满后第 3 笔返回 ErrPoolFull（原因明确，不静默丢弃）")

	// 打包腾出一个位置后，被拒绝的 tx2 重新提交成功。
	st.nonces["alice"] = 1
	p.Reconcile(st, []Tx{tx0}, nil)
	if p.Len() != 1 {
		t.Fatalf("pool len = %d, want 1", p.Len())
	}
	if err := p.Add(tx2); err != nil {
		t.Fatalf("resubmit after free slot: %v", err)
	}
	if p.Len() != 2 {
		t.Fatalf("pool len = %d, want 2", p.Len())
	}
	t.Log("腾出空间后此前被拒绝的交易重新提交成功")

	// 撤销回流不受容量限制（链上事实，不能丢交易）。
	st2 := NewState(map[string]uint64{"alice": 1_000_000})
	p2 := NewTxPoolWithCap(func() *State { return st2 }, 1)
	rv0 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 0}
	rv1 := Tx{From: "alice", To: "bob", Amount: 1, Nonce: 1}
	p2.Reconcile(st2, nil, []Tx{rv0, rv1})
	if p2.Len() != 2 {
		t.Fatalf("pool len = %d, want 2（撤销回流不得因容量丢交易）", p2.Len())
	}
	t.Log("重组撤销回流不受容量上限限制，保证不丢交易")
}
