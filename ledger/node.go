package ledger

import "sync"

// DefaultPoolCapacity 是 NewNode 使用的默认交易池容量上限。
const DefaultPoolCapacity = 1024

// Node 组合链与交易池，对外提供并发安全的接口。
// 并发约束：所有公开方法都通过同一把 sync.RWMutex 串行化，
// 写操作（SubmitTx / ImportBlock / ProduceBlock）互斥且原子，
// 读操作（Balance / Nonce / Head / MainChain）只读到某个
// 完整操作结束后的状态，绝不会读到重组进行中的中间状态。
//
// 池随主链走：任何导致主链顶端变化的操作（导入外部区块触发重组、
// 本地出块）都会在同一临界区内把交易池与新主链状态重新对齐——
// 失效交易清除、被重组撤销的交易自动回池，外界永远看不到
// 「链已切换但池还是旧的」的中间状态。
type Node struct {
	mu    sync.RWMutex
	chain *Chain
	pool  *TxPool
}

// NewNode 以给定创世余额分配创建节点（创世区块自动生成），
// 交易池容量使用 DefaultPoolCapacity。
func NewNode(genesisBalances map[string]uint64) *Node {
	return NewNodeWithPoolCap(genesisBalances, DefaultPoolCapacity)
}

// NewNodeWithPoolCap 创建节点并指定交易池容量上限（<= 0 表示不限）。
func NewNodeWithPoolCap(genesisBalances map[string]uint64, poolCap int) *Node {
	genesis := &Block{Height: 0, Weight: 1}
	genesis.Seal()
	chain := NewChain(genesis, genesisBalances)
	n := &Node{chain: chain}
	n.pool = NewTxPool(chain.State, poolCap)
	return n
}

// SubmitTx 校验并提交交易到交易池。
// nonce 缺口、nonce 已确认、重复交易、余额不足、池满都会返回明确错误；
// 被拒绝的交易不留下任何残留标记，条件满足后重新提交可以成功。
func (n *Node) SubmitTx(tx Tx) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pool.Add(tx)
}

// ImportBlock 校验并导入外部区块；成功后在同一临界区内把交易池
// 与新主链对齐：已上链 / 失效的交易清除，被重组撤销的交易自动回池。
// 校验失败时链状态与交易池均不受影响。
func (n *Node) ImportBlock(b *Block) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	// 导入前记录旧主链，用于之后找出被重组撤销的交易。
	oldMain := n.chain.MainChain()
	if err := n.chain.Import(b); err != nil {
		return err
	}
	n.realignPoolLocked(oldMain)
	return nil
}

// realignPoolLocked 在链状态变化后把池子对齐到新主链（调用方须持有写锁）。
// 旧主链上有而新主链没有的交易即被重组撤销的交易，交给池子回收重排。
func (n *Node) realignPoolLocked(oldMain []*Block) {
	inNew := make(map[[32]byte]struct{})
	for _, blk := range n.chain.MainChain() {
		for _, tx := range blk.Txs {
			inNew[tx.Hash()] = struct{}{}
		}
	}
	var reverted []Tx
	for _, blk := range oldMain {
		for _, tx := range blk.Txs {
			if _, ok := inNew[tx.Hash()]; !ok {
				reverted = append(reverted, tx)
			}
		}
	}
	n.pool.Reorg(n.chain.State(), reverted)
}

// ProduceBlock 把池中全部待打包交易按 nonce 顺序打包成新区块，
// 接到当前主链顶端，并从池中移除这些交易。
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
	if err := n.chain.Import(b); err != nil {
		return nil, err
	}
	n.pool.Remove(b.Txs)
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

// PoolLen 返回交易池中的交易数量。
func (n *Node) PoolLen() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pool.Len()
}

// PoolPending 返回池中全部待打包交易的快照（账户字典序 + nonce 升序）。
func (n *Node) PoolPending() []Tx {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pool.Pending()
}
