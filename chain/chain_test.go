package chain

import (
	"errors"
	"testing"

	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

const (
	a types.Address = "A"
	b types.Address = "B"
	c types.Address = "C"
)

func newTestChain(t *testing.T) *Chain {
	t.Helper()
	gen := types.Block{Height: 0}
	gs := state.New()
	gs.SetBalance(a, 100)
	gs.SetBalance(b, 100)
	gs.SetBalance(c, 0)
	ch, err := NewWithGenesisState(gen, gs)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	return ch
}

func tx(from, to types.Address, nonce, value uint64) types.Transaction {
	return types.Transaction{From: from, To: to, Nonce: nonce, Value: value}
}

func blk(parent types.Hash, height, weight uint64, txs ...types.Transaction) types.Block {
	return types.Block{ParentHash: parent, Height: height, Weight: weight, Txs: txs}
}

// TestReorg 业务：分叉选择与链重组。
func TestReorg(t *testing.T) {
	t.Run("分叉选择_高权重分支胜出且状态等价于直接重放", func(t *testing.T) {
		ch := newTestChain(t)
		gen := ch.genesis.hash

		// 分支一：A->B 10，权重 10。
		ba := blk(gen, 1, 10, tx(a, b, 0, 10))
		if sw, err := ch.Import(ba); err != nil || !sw {
			t.Fatalf("import ba: switched=%v err=%v", sw, err)
		}
		if ch.TipHash() != ba.Hash() {
			t.Fatal("期望 tip=ba")
		}
		if got := ch.State().GetBalance(b); got != 110 {
			t.Fatalf("分支一生效后 B 余额=%d, 期望110", got)
		}
		t.Logf("用例1 分支一(权重10)成为主链: A=%d B=%d",
			ch.State().GetBalance(a), ch.State().GetBalance(b))

		// 分支二：A->C 40，首块权重 5（低于10），为侧链。
		bx := blk(gen, 1, 5, tx(a, c, 0, 40))
		if sw, err := ch.Import(bx); err != nil {
			t.Fatalf("import bx: %v", err)
		} else if sw {
			t.Fatal("低权重侧链不应切换主链")
		}
		t.Logf("用例2 分支二首块(权重5)为侧链，主链不变: A=%d C=%d",
			ch.State().GetBalance(a), ch.State().GetBalance(c))

		// 分支二续块权重 20：累积 25 > 10，触发重组。
		bx2 := blk(bx.Hash(), 2, 20, tx(c, a, 0, 5))
		sw, err := ch.Import(bx2)
		if err != nil {
			t.Fatalf("import bx2: %v", err)
		}
		if !sw {
			t.Fatal("更高累积权重分支应触发重组切换")
		}
		t.Log("用例3 分支二累积权重25>10，主链切换，旧分支撤销/新分支重放")

		// 重组后：A 先转出40(余60)，C 转回5 => A=65, B=100(撤销后回到初始), C=35。
		st := ch.State()
		if st.GetBalance(a) != 65 || st.GetBalance(b) != 100 || st.GetBalance(c) != 35 {
			t.Fatalf("重组后余额错误: A=%d B=%d C=%d", st.GetBalance(a), st.GetBalance(b), st.GetBalance(c))
		}
		if st.GetNonce(a) != 1 || st.GetNonce(c) != 1 || st.GetNonce(b) != 0 {
			t.Fatalf("重组后 nonce 错误: A=%d B=%d C=%d", st.GetNonce(a), st.GetNonce(b), st.GetNonce(c))
		}
		// 主链哈希序列应为 创世,bx,bx2。
		hs := ch.CanonicalHashes()
		if len(hs) != 3 || hs[0] != gen || hs[1] != bx.Hash() || hs[2] != bx2.Hash() {
			t.Fatalf("主链序列错误: %x", hs)
		}
		// 关键判据：顶端缓存状态 == 从创世按主链完整重放状态。
		if !ch.VerifyTipState() {
			t.Fatal("重组后顶端状态与完整重放不一致（存在重复计算/丢失）")
		}
		t.Logf("用例4 重组后余额 A=%d B=%d C=%d；顶端状态==全量重放: %v",
			st.GetBalance(a), st.GetBalance(b), st.GetBalance(c), ch.VerifyTipState())
	})

	t.Run("累积权重相同_末端哈希字节序较小者胜出", func(t *testing.T) {
		ch := newTestChain(t)
		gen := ch.genesis.hash
		// 两个等高等权重分叉，仅交易不同（哈希不同）。
		f1 := blk(gen, 1, 7, tx(a, b, 0, 1))
		f2 := blk(gen, 1, 7, tx(a, c, 0, 1))
		if _, err := ch.Import(f1); err != nil {
			t.Fatal(err)
		}
		if _, err := ch.Import(f2); err != nil {
			t.Fatal(err)
		}
		want := f1.Hash()
		if compareHash(f1.Hash(), f2.Hash()) > 0 {
			want = f2.Hash()
		}
		if ch.TipHash() != want {
			t.Fatalf("平手决胜错误: tip=%s 期望=%s", ch.TipHash().Hex(), want.Hex())
		}
		t.Logf("用例5 等权重平手，按末端哈希决胜: tip=%s", ch.TipHash().Hex())
	})

	t.Run("再次延长旧分支权重反超_可二次重组回去", func(t *testing.T) {
		ch := newTestChain(t)
		gen := ch.genesis.hash
		f1 := blk(gen, 1, 10, tx(a, b, 0, 10))
		f2 := blk(gen, 1, 5, tx(a, c, 0, 20))
		if _, err := ch.Import(f1); err != nil {
			t.Fatal(err)
		}
		if _, err := ch.Import(f2); err != nil {
			t.Fatal(err)
		}
		f2c := blk(f2.Hash(), 2, 20, tx(c, a, 0, 20))
		if sw, err := ch.Import(f2c); err != nil || !sw { // 切到 f2 链
			t.Fatalf("import f2c: switched=%v err=%v", sw, err)
		}
		f1c := blk(f1.Hash(), 2, 30, tx(b, a, 0, 10))
		if sw, err := ch.Import(f1c); err != nil || !sw { // f1 链累积 40 反超，切回去
			t.Fatalf("import f1c: switched=%v err=%v", sw, err)
		}
		st := ch.State()
		// f1: A->B 10 => A90 B110；f1c: B->A 10 => A100 B100。
		if st.GetBalance(a) != 100 || st.GetBalance(b) != 100 || st.GetBalance(c) != 0 {
			t.Fatalf("二次重组后余额错误: A=%d B=%d C=%d",
				st.GetBalance(a), st.GetBalance(b), st.GetBalance(c))
		}
		if !ch.VerifyTipState() {
			t.Fatal("二次重组后状态与重放不一致")
		}
		t.Logf("用例6 二次重组后余额 A=%d B=%d C=%d，顶端状态==全量重放: %v",
			st.GetBalance(a), st.GetBalance(b), st.GetBalance(c), ch.VerifyTipState())
	})
}

// TestImportValidation 业务：区块导入校验，各类拒绝原因必须可区分。
func TestImportValidation(t *testing.T) {
	ch := newTestChain(t)
	gen := ch.genesis.hash

	t.Run("父区块缺失_乱序到达被拒绝且不影响链", func(t *testing.T) {
		orphan := blk(types.Hash{0xAB, 0xCD}, 5, 1, tx(a, b, 0, 1))
		_, err := ch.Import(orphan)
		if !errors.Is(err, ErrParentMissing) {
			t.Fatalf("期望 ErrParentMissing, 实际 %v", err)
		}
		if ch.Height() != 0 || ch.HasBlock(orphan.Hash()) {
			t.Fatal("乱序区块不得写入树或改变高度")
		}
		t.Logf("用例 乱序区块被拒绝(父缺失): %v；链高仍为 %d", err, ch.Height())
	})

	t.Run("高度错乱被拒绝", func(t *testing.T) {
		bad := blk(gen, 9, 1, tx(a, b, 0, 1)) // 应为高度1
		_, err := ch.Import(bad)
		if !errors.Is(err, ErrHeightMismatch) {
			t.Fatalf("期望 ErrHeightMismatch, 实际 %v", err)
		}
		t.Logf("用例 高度错乱被拒绝: %v", err)
	})

	t.Run("内容篡改_公告哈希与重算不符", func(t *testing.T) {
		good := blk(gen, 1, 1, tx(a, b, 0, 1))
		tampered := good
		tampered.Txs = []types.Transaction{tx(a, b, 0, 99)} // 篡改交易金额
		ann := types.Announce{Block: tampered, Hash: good.Hash()}
		_, err := ch.ImportAnnounced(ann)
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("期望 ErrTampered, 实际 %v", err)
		}
		if ch.HasBlock(tampered.Hash()) {
			t.Fatal("被篡改区块不得写入树")
		}
		t.Logf("用例 内容篡改被拒绝: %v", err)
	})

	t.Run("重复导入被拒绝", func(t *testing.T) {
		good := blk(gen, 1, 1, tx(a, b, 0, 1))
		if _, err := ch.Import(good); err != nil {
			t.Fatal(err)
		}
		_, err := ch.Import(good)
		if !errors.Is(err, ErrDuplicate) {
			t.Fatalf("期望 ErrDuplicate, 实际 %v", err)
		}
		t.Logf("用例 重复导入被拒绝: %v", err)
	})

	t.Run("区块内非法交易_整块拒绝且链状态不变", func(t *testing.T) {
		// 当前主链为高度1的 good，A 的 nonce 已推进到1、余额99。
		bad := blk(ch.TipHash(), 2, 1, tx(a, b, 0, 1)) // nonce 重复
		_, err := ch.Import(bad)
		if !errors.Is(err, ErrInvalidTx) {
			t.Fatalf("期望 ErrInvalidTx, 实际 %v", err)
		}
		// 余额不足同样归类为非法交易。
		bad2 := blk(ch.TipHash(), 2, 1, tx(a, b, 1, 100000))
		if _, err := ch.Import(bad2); !errors.Is(err, ErrInvalidTx) {
			t.Fatalf("期望 ErrInvalidTx(余额不足), 实际 %v", err)
		}
		if ch.Height() != 1 {
			t.Fatalf("非法块不得改变高度, 实际 %d", ch.Height())
		}
		t.Logf("用例 非法交易整块拒绝(nonce重复/余额不足), 链高仍为 %d", ch.Height())
	})

	t.Run("合法乱序补齐_先到子块被拒父块到后再导入成功", func(t *testing.T) {
		ch2 := newTestChain(t)
		g := ch2.genesis.hash
		mid := blk(g, 1, 1, tx(a, b, 0, 5))
		next := blk(mid.Hash(), 2, 1, tx(a, b, 1, 5))
		if _, err := ch2.Import(next); !errors.Is(err, ErrParentMissing) {
			t.Fatalf("先到子块应报父缺失, 实际 %v", err)
		}
		if _, err := ch2.Import(mid); err != nil {
			t.Fatal(err)
		}
		if _, err := ch2.Import(next); err != nil {
			t.Fatal(err)
		}
		if ch2.Height() != 2 || ch2.State().GetBalance(b) != 110 {
			t.Fatalf("补齐后状态错误: height=%d B=%d", ch2.Height(), ch2.State().GetBalance(b))
		}
		t.Logf("用例 乱序补齐成功: 链高=%d B余额=%d", ch2.Height(), ch2.State().GetBalance(b))
	})
}
