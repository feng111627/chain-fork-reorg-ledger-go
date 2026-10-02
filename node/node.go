// Package node 组合账本状态、区块链与交易池，提供并发安全的节点门面。
//
// 并发约束：
//   - 节点内部用一把 sync.RWMutex 串行化所有会读取/改变链状态的操作；
//   - 区块导入、交易提交、状态查询、出块打包在同一把锁下互斥，
//     因此外部永远读不到“导入一半 / 重组一半”的中间状态；
//   - 状态查询使用读锁，可彼此并行；导入与提交使用写锁。
package node

import (
	"sync"

	"github.com/feng111627/chain-fork-reorg-ledger-go/chain"
	"github.com/feng111627/chain-fork-reorg-ledger-go/pool"
	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

// Node 本地链式账本节点。所有方法可被多 goroutine 并发调用。
type Node struct {
	mu   sync.RWMutex
	ch   *chain.Chain
	pool *pool.Pool
}

// New 以创世区块创建节点（创世块交易用于初始分配）。
func New(genesis types.Block) (*Node, error) {
	ch, err := chain.New(genesis)
	if err != nil {
		return nil, err
	}
	return &Node{
		ch:   ch,
		pool: pool.New(ch.State()),
	}, nil
}

// NewWithGenesisState 以创世块与显式初始状态创建节点。
func NewWithGenesisState(genesis types.Block, genesisState *state.State) (*Node, error) {
	ch, err := chain.NewWithGenesisState(genesis, genesisState)
	if err != nil {
		return nil, err
	}
	return &Node{
		ch:   ch,
		pool: pool.New(ch.State()),
	}, nil
}

// SubmitTx 校验并提交交易到交易池（写锁）。
// nonce 缺口/重复、余额不足、发送方缺失都会返回可区分的错误，
// 而不是被静默丢弃。
func (n *Node) SubmitTx(tx types.Transaction) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pool.Add(tx)
}

// ImportBlock 导入区块（写锁）。
// 当区块延长或改变了主链（switched==true，包括正常延长与重组）时，
// 用新的主链顶端状态刷新交易池：已打包交易自然被移除，
// 重组后不再合法的池内交易被清除，其余交易按新基准重新校验保留。
// 区块只是挂到未成为主链的侧分叉时，链与池均不改变对外状态。
func (n *Node) ImportBlock(b types.Block) (switched bool, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switched, err = n.ch.Import(b)
	if err != nil {
		return false, err
	}
	if switched {
		n.pool.OnNewTip(n.ch.State())
	}
	return switched, nil
}

// ImportAnnounced 导入带声明哈希的区块公告（网络场景），
// 内容被篡改会得到 chain.ErrTampered。
func (n *Node) ImportAnnounced(ann types.Announce) (switched bool, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switched, err = n.ch.ImportAnnounced(ann)
	if err != nil {
		return false, err
	}
	if switched {
		n.pool.OnNewTip(n.ch.State())
	}
	return switched, nil
}

// ProposeBlock 基于当前主链顶端与池中可打包交易构造下一个候选区块（写锁）。
// 注意：这只是“提议/打包”，不会自动导入；调用方随后调用 ImportBlock
// 才使其生效（模拟节点间出块、竞争与可能的重组）。
func (n *Node) ProposeBlock(weight uint64) types.Block {
	n.mu.Lock()
	defer n.mu.Unlock()
	return types.Block{
		ParentHash: n.ch.TipHash(),
		Height:     n.ch.Height() + 1,
		Weight:     weight,
		Txs:        n.pool.Pending(),
	}
}

// CommitBlock 提议并立即导入一个区块（写锁），返回最终上链的区块。
// 导入成功后其中交易即从交易池移除。
func (n *Node) CommitBlock(weight uint64) (types.Block, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	b := types.Block{
		ParentHash: n.ch.TipHash(),
		Height:     n.ch.Height() + 1,
		Weight:     weight,
		Txs:        n.pool.Pending(),
	}
	if _, err := n.ch.Import(b); err != nil {
		return types.Block{}, err
	}
	n.pool.OnNewTip(n.ch.State())
	return b, nil
}

// Balance 查询账户余额（读锁）。
func (n *Node) Balance(addr types.Address) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ch.State().GetBalance(addr)
}

// Nonce 查询账户当前 nonce（读锁）。
func (n *Node) Nonce(addr types.Address) uint64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ch.State().GetNonce(addr)
}

// TipInfo 返回主链顶端高度、哈希摘要与累积权重（读锁）。
func (n *Node) TipInfo() (uint64, string, uint64) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ch.Height(), n.ch.TipHash().Hex(), n.ch.TipWeight()
}

// CanonicalHashes 返回主链哈希摘要序列（读锁）。
func (n *Node) CanonicalHashes() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	hs := n.ch.CanonicalHashes()
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Hex()
	}
	return out
}

// Canonical 主链区块序列（读锁）。
func (n *Node) Canonical() []types.Block {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ch.Canonical()
}

// PendingTxs 返回池中待打包交易的有序副本（读锁）。
func (n *Node) PendingTxs() []types.Transaction {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pool.Content()
}

// PoolSize 返回交易池大小（读锁）。
func (n *Node) PoolSize() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.pool.Size()
}

// VerifyTipState 核对顶端状态与全量重放是否一致（读锁）。
func (n *Node) VerifyTipState() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ch.VerifyTipState()
}

// StateSnapshot 返回主链顶端状态的深拷贝（读锁）。
func (n *Node) StateSnapshot() *state.State {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ch.State()
}

// Chain 暴露底层链（测试与演示用）。
func (n *Node) Chain() *chain.Chain { return n.ch }

// Pool 暴露底层交易池（测试与演示用；调用方需自行确保处于锁定语义下）。
func (n *Node) Pool() *pool.Pool { return n.pool }

// Lock / Unlock 仅供测试在需要跨多次调用保持原子视图时使用。
func (n *Node) Lock()   { n.mu.Lock() }
func (n *Node) Unlock() { n.mu.Unlock() }

// ProposeBlockWith 基于当前顶端构造候选区块并使用给定交易（不自动取池）。
// txs 为 nil 时生成空块。仅构造不导入，调用方随后走 ImportBlock。
func (n *Node) ProposeBlockWith(weight uint64, txs []types.Transaction) types.Block {
	n.mu.Lock()
	defer n.mu.Unlock()
	return types.Block{
		ParentHash: n.ch.TipHash(),
		Height:     n.ch.Height() + 1,
		Weight:     weight,
		Txs:        txs,
	}
}
