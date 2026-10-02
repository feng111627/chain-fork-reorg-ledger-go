// 本地链式账本节点演示入口。
//
// 运行： go run .
//
// 演示内容：
//  1. 创世分配；
//  2. 两条分叉先后成为主链，高权重分支触发重组；
//  3. 重组后账户状态与“直接按主链重放”完全一致；
//  4. 交易池对 nonce 缺口、余额不足的明确拒绝；
//  5. 交易打包后从池中移除。
package main

import (
	"errors"
	"fmt"

	"github.com/feng111627/chain-fork-reorg-ledger-go/chain"
	"github.com/feng111627/chain-fork-reorg-ledger-go/node"
	"github.com/feng111627/chain-fork-reorg-ledger-go/pool"
	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

func main() {
	const (
		alice types.Address = "Alice"
		bob   types.Address = "Bob"
		carol types.Address = "Carol"
	)

	genesis := types.Block{Height: 0, Weight: 0}
	gs := state.New()
	gs.SetBalance(alice, 100)
	gs.SetBalance(bob, 50)
	gs.SetBalance(carol, 0)

	n, err := node.NewWithGenesisState(genesis, gs)
	must(err)

	fmt.Println("== 初始状态 ==")
	printBalances(n, alice, bob, carol)

	// 分叉 A：Alice -> Bob 30（权重 10）。
	forkA := types.Block{
		ParentHash: n.Chain().TipHash(),
		Height:     1,
		Weight:     10,
		Txs: []types.Transaction{
			{From: alice, To: bob, Nonce: 0, Value: 30},
		},
	}
	switched, err := n.ImportBlock(forkA)
	must(err)
	fmt.Printf("\n== 导入分叉A(高度1,权重10) switched=%v ==\n", switched)
	printBalances(n, alice, bob, carol)

	// 分叉 B 从创世分叉：Alice -> Carol 90（权重 5），当前权重较小，仍是侧链。
	forkB := types.Block{
		ParentHash: genesis.Hash(),
		Height:     1,
		Weight:     5,
		Txs: []types.Transaction{
			{From: alice, To: carol, Nonce: 0, Value: 90},
		},
	}
	switched, err = n.ImportBlock(forkB)
	must(err)
	fmt.Printf("\n== 导入分叉B(高度1,权重5,更低权重侧链) switched=%v ==\n", switched)
	printBalances(n, alice, bob, carol)

	// 分叉 B 续块，累积权重 5+20=25 > 10，触发重组：撤销A、生效B。
	forkB2 := types.Block{
		ParentHash: forkB.Hash(),
		Height:     2,
		Weight:     20,
		Txs: []types.Transaction{
			{From: carol, To: alice, Nonce: 0, Value: 20},
		},
	}
	switched, err = n.ImportBlock(forkB2)
	must(err)
	fmt.Printf("\n== 导入分叉B续块(权重20,累积25>10) switched=%v，触发重组 ==\n", switched)
	printBalances(n, alice, bob, carol)

	h, hh, w := n.TipInfo()
	fmt.Printf("主链顶端: 高度=%d 哈希=%s 累积权重=%d\n", h, hh, w)
	fmt.Printf("主链哈希序列: %v\n", n.CanonicalHashes())
	fmt.Printf("顶端状态 == 全量重放状态: %v\n", n.VerifyTipState())

	// 交易池校验演示。
	// 当前主链(B,B2)上 Alice 已执行 nonce=0（转给Carol 90），余额 30，期望 nonce=1。
	fmt.Println("\n== 交易池校验 ==")
	tryTx(n, types.Transaction{From: alice, To: bob, Nonce: 2, Value: 1},
		"nonce 缺口(直接提交 nonce=2)")
	tryTx(n, types.Transaction{From: alice, To: bob, Nonce: 1, Value: 1000},
		"余额不足(花1000)")
	tryTx(n, types.Transaction{From: alice, To: carol, Nonce: 1, Value: 10},
		"合法交易(nonce=1 花10)")
	tryTx(n, types.Transaction{From: alice, To: bob, Nonce: 1, Value: 5},
		"重复交易(同 nonce 再提交)")
	fmt.Printf("当前待打包交易数: %d\n", n.PoolSize())

	fmt.Println("\n== 打包出块 ==")
	b, err := n.CommitBlock(2)
	must(err)
	fmt.Printf("已提交区块 高度=%d 交易数=%d，打包后池中剩余=%d\n",
		b.Height, len(b.Txs), n.PoolSize())
	printBalances(n, alice, bob, carol)
	fmt.Printf("顶端状态 == 全量重放状态: %v\n", n.VerifyTipState())
}

func tryTx(n *node.Node, tx types.Transaction, label string) {
	err := n.SubmitTx(tx)
	if err != nil {
		reason := classify(err)
		fmt.Printf("[拒绝] %-28s 原因=%s (%v)\n", label, reason, err)
		return
	}
	fmt.Printf("[接受] %s\n", label)
}

func classify(err error) string {
	switch {
	case errors.Is(err, chain.ErrDuplicate):
		return "重复区块"
	case errors.Is(err, chain.ErrParentMissing):
		return "父区块缺失"
	case errors.Is(err, chain.ErrHeightMismatch):
		return "高度错乱"
	case errors.Is(err, chain.ErrTampered):
		return "内容篡改"
	case errors.Is(err, chain.ErrInvalidTx):
		return "区块内非法交易"
	case errors.Is(err, state.ErrNonceMismatch):
		return "nonce 不匹配"
	case errors.Is(err, state.ErrInsufficientBalance):
		return "余额不足"
	case errors.Is(err, state.ErrSenderMissing):
		return "发送方不存在"
	case errors.Is(err, pool.ErrAlreadyPending):
		return "池中已存在同nonce交易"
	default:
		return "其它"
	}
}

func printBalances(n *node.Node, addrs ...types.Address) {
	for _, a := range addrs {
		fmt.Printf("  %-5s 余额=%-4d nonce=%d\n", a, n.Balance(a), n.Nonce(a))
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
