package pool

import (
	"errors"
	"testing"

	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

const (
	x types.Address = "X"
	y types.Address = "Y"
)

func baseState() *state.State {
	st := state.New()
	st.SetBalance(x, 100)
	st.SetBalance(y, 0)
	return st
}

// TestPool 业务：交易池的 nonce 顺序、余额投影、移除与重组后刷新。
func TestPool(t *testing.T) {
	t.Run("nonce必须连续且余额含池内支出", func(t *testing.T) {
		p := New(baseState())

		if err := p.Add(types.Transaction{From: x, To: y, Nonce: 1, Value: 1}); !errors.Is(err, state.ErrNonceMismatch) {
			t.Fatalf("缺口 nonce 应拒绝: %v", err)
		}
		t.Log("用例 nonce 缺口被明确拒绝(ErrNonceMismatch)")

		if err := p.Add(types.Transaction{From: x, To: y, Nonce: 0, Value: 100}); err != nil {
			t.Fatal(err)
		}
		t.Log("用例 nonce0 花100 入池成功")

		// 池内已花100，基准余额100：再花1必余额不足。
		if err := p.Add(types.Transaction{From: x, To: y, Nonce: 1, Value: 1}); !errors.Is(err, state.ErrInsufficientBalance) {
			t.Fatalf("投影余额不足应拒绝: %v", err)
		}
		t.Log("用例 叠加池内支出后余额不足被拒绝(ErrInsufficientBalance)")

		if err := p.Add(types.Transaction{From: "Nobody", To: y, Nonce: 0, Value: 1}); !errors.Is(err, state.ErrSenderMissing) {
			t.Fatalf("发送方缺失应拒绝: %v", err)
		}
		t.Log("用例 发送方不存在被拒绝(ErrSenderMissing)")

	})

	t.Run("Pending顺序确定_按发送方与nonce", func(t *testing.T) {
		p := New(baseState())
		// 另给 y 注资以便 y 也能发起交易。
		st := baseState()
		st.SetBalance(y, 50)
		p = New(st)
		must(t, p.Add(types.Transaction{From: x, To: y, Nonce: 0, Value: 5}))
		must(t, p.Add(types.Transaction{From: y, To: x, Nonce: 0, Value: 2}))
		must(t, p.Add(types.Transaction{From: x, To: y, Nonce: 1, Value: 5}))

		pend := p.Pending()
		if len(pend) != 3 {
			t.Fatalf("期望3笔, 实际 %d", len(pend))
		}
		if pend[0].From != x || pend[0].Nonce != 0 ||
			pend[1].From != x || pend[1].Nonce != 1 ||
			pend[2].From != y || pend[2].Nonce != 0 {
			t.Fatalf("Pending 顺序不确定/错误: %+v", pend)
		}
		t.Logf("用例 Pending 按(发送方,nonce)排序: X0,X1,Y0")
	})

	t.Run("Remove后重建投影", func(t *testing.T) {
		p := New(baseState())
		t0 := types.Transaction{From: x, To: y, Nonce: 0, Value: 60}
		must(t, p.Add(t0))
		// 此时 t1=50 在投影余额(40)下不足，无法入池。
		if err := p.Add(types.Transaction{From: x, To: y, Nonce: 1, Value: 50}); !errors.Is(err, state.ErrInsufficientBalance) {
			t.Fatalf("应余额不足: %v", err)
		}
		p.Remove(t0) // 模拟 t0 被打包移除
		// 移除后基准仍为初始（池不感知上链，本用例仅测投影重建）：t1 的 nonce0 恢复可用。
		if err := p.Add(types.Transaction{From: x, To: y, Nonce: 0, Value: 50}); err != nil {
			t.Fatalf("移除重建后应可入池: %v", err)
		}
		t.Log("用例 Remove 后投影重建，余额可用性恢复")
	})

	t.Run("OnNewTip重组后_已落账交易被清除", func(t *testing.T) {
		p := New(baseState())
		must(t, p.Add(types.Transaction{From: x, To: y, Nonce: 0, Value: 10}))
		must(t, p.Add(types.Transaction{From: x, To: y, Nonce: 1, Value: 10}))

		// 新主链上 x 已执行 nonce0=20：通过在初始状态上执行该交易，
		// 构造出“重组后顶端”的基准状态（x 余额80、nonce1）。
		newTip := state.New()
		newTip.SetBalance(x, 100)
		must(t, newTip.Apply(types.Transaction{From: x, To: y, Nonce: 0, Value: 20}))
		p.OnNewTip(newTip)

		// 池中原 nonce0 交易应已清除（nonce 已推进）；nonce1 交易仍合法保留。
		if p.Size() != 1 {
			t.Fatalf("重组后池中应保留1笔(nonce1), 实际 %d: %+v", p.Size(), p.Content())
		}
		if p.Content()[0].Nonce != 1 {
			t.Fatalf("保留的应为 nonce1, 实际 %d", p.Content()[0].Nonce)
		}
		t.Log("用例 OnNewTip 重组后已落账(nonce0)交易清除，nonce1 保留")
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
