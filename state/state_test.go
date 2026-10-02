package state

import (
	"errors"
	"testing"

	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

// TestState 业务：账户状态机的 nonce 推进、余额转移、克隆隔离与守恒。
func TestState(t *testing.T) {
	const (
		p types.Address = "P"
		q types.Address = "Q"
	)

	t.Run("转账推进nonce并守恒", func(t *testing.T) {
		s := New()
		s.SetBalance(p, 100)
		if err := s.Apply(types.Transaction{From: p, To: q, Nonce: 0, Value: 30}); err != nil {
			t.Fatal(err)
		}
		if s.GetBalance(p) != 70 || s.GetBalance(q) != 30 || s.GetNonce(p) != 1 {
			t.Fatalf("转账后状态错误: P=%d Q=%d nonceP=%d", s.GetBalance(p), s.GetBalance(q), s.GetNonce(p))
		}
		t.Logf("用例 转账30成功: P=%d Q=%d nonceP=%d，总额守恒=100", s.GetBalance(p), s.GetBalance(q), s.GetNonce(p))
	})

	t.Run("三类校验错误可区分", func(t *testing.T) {
		s := New()
		s.SetBalance(p, 10)
		if err := s.Apply(types.Transaction{From: "Nobody", To: q, Nonce: 0, Value: 1}); !errors.Is(err, ErrSenderMissing) {
			t.Fatalf("期望 ErrSenderMissing: %v", err)
		}
		if err := s.Apply(types.Transaction{From: p, To: q, Nonce: 5, Value: 1}); !errors.Is(err, ErrNonceMismatch) {
			t.Fatalf("期望 ErrNonceMismatch: %v", err)
		}
		if err := s.Apply(types.Transaction{From: p, To: q, Nonce: 0, Value: 11}); !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("期望 ErrInsufficientBalance: %v", err)
		}
		if s.GetBalance(p) != 10 || s.GetNonce(p) != 0 {
			t.Fatal("校验失败不得改变状态")
		}
		t.Log("用例 发送方缺失/nonce错乱/余额不足 三类错误均可区分且不改状态")
	})

	t.Run("Clone分叉状态互相隔离", func(t *testing.T) {
		s := New()
		s.SetBalance(p, 100)
		fork := s.Clone()
		if err := fork.Apply(types.Transaction{From: p, To: q, Nonce: 0, Value: 40}); err != nil {
			t.Fatal(err)
		}
		if fork.GetBalance(p) != 60 {
			t.Fatalf("分叉余额=%d 期望60", fork.GetBalance(p))
		}
		if s.GetBalance(p) != 100 || s.GetNonce(p) != 0 {
			t.Fatal("克隆上的执行泄漏到了原状态")
		}
		if s.Equal(fork) {
			t.Fatal("两分叉状态不应相等")
		}
		t.Log("用例 Clone 后分叉执行互不污染，Equal 正确判异")
	})
}
