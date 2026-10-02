package ledger

import "sync"

// Node 组合链与交易池，对外提供并发安全的接口。
// 并发约束：所有公开方法都通过同一把 sync.RWMutex 串行化，
// 写操作（SubmitTx / ImportBlock / ProduceBlock）互斥且原子，
// 读操作（Balance / Nonce / Head / MainChain）只读到某个
// 完整操作结束后的状态，绝不会读到重组进行中的中间状态。
type Node struct {
	mu    sync.RWMutex
	chain *Chain
	pool  *TxPool
}

// NewNode 以给定创世余额分配创建节点（创世区块自动生成）。
func NewNode(genesisBalances map[string]uint64) *Node {
	return NewNodeWithCap(genesisBalances, DefaultPoolCap)
}

// NewNodeWithCap 创建带交易池容量上限的节点；cap<=0 时使用默认容量。
func NewNodeWithCap(genesisBalances map[string]uint64, cap int) *Node {
	genesis := &Block{Height: 0, Weight: 1}
	genesis.Seal()
	chain := NewChain(genesis, genesisBalances)
	n := &Node{chain: chain}
	n.pool = NewTxPoolWithCap(chain.State, cap)
	return n
}

// SubmitTx 校验并提交交易到交易池。
// nonce 缺口、nonce 已确认、重复交易、余额不足都会返回明确错误。
func (n *Node) SubmitTx(tx Tx) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pool.Add(tx)
}

// ImportBlock 校验并导入外部区块；成功后按主链差异让交易池与新顶端原子对齐：
// 新主链已确认的交易移出池子，旧主链被撤销的交易（新主链没有的）回流
// 待打包集合，其余交易按新状态重新校验。区块仅构成侧链时池子完全不动；
// 校验失败时链状态与交易池也都不受影响。
func (n *Node) ImportBlock(b *Block) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	diff, err := n.chain.ImportResult(b)
	if err != nil {
		return err
	}
	n.pool.Reconcile(n.chain.State(), diff.Confirmed, diff.Reverted)
	return nil
}

// ProduceBlock 把池中全部待打包交易按 nonce 顺序打包成新区块，
// 接到当前主链顶端，并按主链差异与池子原子对账。
func (n *Node) ProduceBlock() (*Block, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	head := n.chain.Head()
	b := &Block{
		Height: head.Height + 1,
		Parent: head.BlockHash,
		Txs:    n.pool.Pending(),
		Weight: 1,
	}
	b.Seal()
	diff, err := n.chain.ImportResult(b)
	if err != nil {
		return nil, err
	}
	n.pool.Reconcile(n.chain.State(), diff.Confirmed, diff.Reverted)
	return b, nil
}

// Snapshot 返回主链顶端状态的完整快照（深拷贝）。
// 需要同时观察多个账户时用它保证读到同一时点的状态。
func (n *Node) Snapshot() *State {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.chain.State()
}

// Balance 查询账户余额（主链顶端状态）。
func (n *Node) Balance(addr string) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.chain.State().Balance(addr)
}

// Nonce 查询账户下一笔期望 nonce（主链顶端状态）。
func (n *Node) Nonce(addr string) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.chain.State().Nonce(addr)
}

// Head 返回主链顶端区块哈希与高度。
func (n *Node) Head() ([32]byte, uint64) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	h := n.chain.Head()
	return h.BlockHash, h.Height
}

// MainChain 返回从创世到主链顶端的区块序列快照。
func (n *Node) MainChain() []*Block {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.chain.MainChain()
}

// Pending 返回池中待打包交易的快照（账户字典序 + nonce 升序）。
func (n *Node) Pending() []Tx {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pool.Pending()
}

// PoolLen 返回交易池中的交易数量。
func (n *Node) PoolLen() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pool.Len()
}
