// 本程序演示本地链式账本节点：出块、分叉、重组与状态一致性校验。
package main

import (
	"fmt"

	"github.com/feng111627/chain-fork-reorg-ledger-go/ledger"
)

func child(parent *ledger.Block, weight uint64, txs ...ledger.Tx) *ledger.Block {
	b := &ledger.Block{
		Height: parent.Height + 1,
		Parent: parent.BlockHash,
		Txs:    txs,
		Weight: weight,
	}
	b.Seal()
	return b
}

func printState(n *ledger.Node, addrs ...string) {
	snap := n.Snapshot()
	_, h := n.Head()
	fmt.Printf("  [高度 %d]", h)
	for _, a := range addrs {
		fmt.Printf(" %s=%d(nonce %d)", a, snap.Balance(a), snap.Nonce(a))
	}
	fmt.Println()
}

func main() {
	addrs := []string{"alice", "bob", "carol"}
	n := ledger.NewNode(map[string]uint64{"alice": 100, "bob": 50})

	fmt.Println("== 1. 提交交易并本地出块 ==")
	for i := uint64(0); i < 2; i++ {
		if err := n.SubmitTx(ledger.Tx{From: "alice", To: "carol", Amount: 10, Nonce: i}); err != nil {
			panic(err)
		}
	}
	if _, err := n.ProduceBlock(); err != nil {
		panic(err)
	}
	fmt.Printf("  打包后交易池剩余: %d\n", n.PoolLen())
	printState(n, addrs...)

	fmt.Println("== 2. 构造更高权重的分叉分支触发重组 ==")
	main := n.MainChain()
	genesis := main[0]
	// 分支 B 从创世分叉：三个区块，总权重超过当前主链。
	b1 := child(genesis, 1, ledger.Tx{From: "bob", To: "carol", Amount: 20, Nonce: 0})
	b2 := child(b1, 1, ledger.Tx{From: "alice", To: "bob", Amount: 30, Nonce: 0})
	b3 := child(b2, 2, ledger.Tx{From: "alice", To: "carol", Amount: 5, Nonce: 1})
	for _, b := range []*ledger.Block{b1, b2, b3} {
		if err := n.ImportBlock(b); err != nil {
			panic(err)
		}
	}
	fmt.Println("  导入 B 分支后（旧分支 alice->carol 的两笔转账被撤销）:")
	printState(n, addrs...)

	fmt.Println("== 3. 非法区块被拒绝（原因可区分）==")
	tampered := child(b3, 1, ledger.Tx{From: "alice", To: "carol", Amount: 1, Nonce: 2})
	tampered.Txs[0].Amount = 999 // 篡改内容但不重算哈希
	fmt.Printf("  篡改区块: %v\n", n.ImportBlock(tampered))
	orphan := &ledger.Block{Height: 99, Weight: 1}
	orphan.Seal()
	fmt.Printf("  父缺失区块: %v\n", n.ImportBlock(orphan))
	fmt.Printf("  重复导入: %v\n", n.ImportBlock(b3))

	fmt.Println("== 4. 交易池校验 ==")
	fmt.Printf("  nonce 缺口: %v\n", n.SubmitTx(ledger.Tx{From: "bob", To: "alice", Amount: 1, Nonce: 9}))
	fmt.Printf("  余额不足: %v\n", n.SubmitTx(ledger.Tx{From: "bob", To: "alice", Amount: 100000, Nonce: 1}))

	fmt.Println("== 5. 重组撤销的交易自动回流并重新打包 ==")
	n2 := ledger.NewNode(map[string]uint64{"alice": 100})
	tx := ledger.Tx{From: "alice", To: "carol", Amount: 10, Nonce: 0}
	if err := n2.SubmitTx(tx); err != nil {
		panic(err)
	}
	if _, err := n2.ProduceBlock(); err != nil {
		panic(err)
	}
	fmt.Printf("  旧主链确认后池子: %d, carol=%d\n", n2.PoolLen(), n2.Balance("carol"))
	// 从创世造一条不含该交易、权重更高的分叉触发重组。
	g2 := n2.MainChain()[0]
	f1 := child(g2, 1) // 空块
	f2 := child(f1, 3) // 加大权重反超
	if err := n2.ImportBlock(f1); err != nil {
		panic(err)
	}
	if err := n2.ImportBlock(f2); err != nil {
		panic(err)
	}
	fmt.Printf("  重组后：carol=%d（旧转账撤销），池子=%d（被撤销交易回流）\n",
		n2.Balance("carol"), n2.PoolLen())
	if _, err := n2.ProduceBlock(); err != nil {
		panic(err)
	}
	fmt.Printf("  回流交易重新打包：池子=%d, carol=%d（恰好生效一次）\n",
		n2.PoolLen(), n2.Balance("carol"))

	fmt.Println("== 6. 池子容量上限：打满拒绝，腾出后恢复 ==")
	n3 := ledger.NewNodeWithCap(map[string]uint64{"alice": 1_000_000}, 2)
	for i := uint64(0); i < 3; i++ {
		err := n3.SubmitTx(ledger.Tx{From: "alice", To: "bob", Amount: 1, Nonce: i})
		if err != nil {
			fmt.Printf("  nonce=%d 提交被拒: %v\n", i, err)
		}
	}
	if _, err := n3.ProduceBlock(); err != nil {
		panic(err)
	}
	fmt.Printf("  出块腾出空间后池子: %d，再提交 nonce=2: %v\n",
		n3.PoolLen(), n3.SubmitTx(ledger.Tx{From: "alice", To: "bob", Amount: 1, Nonce: 2}))
}
