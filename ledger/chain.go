package ledger

import (
	"bytes"
	"fmt"
)

// Chain 维护区块集合、主链选择与重组。
// 分叉选择规则（可解释、确定性）：
//  1. 累积权重（从创世到区块的 Weight 之和）高者为主链；
//  2. 权重相同时，区块哈希字典序小者为主链。
//
// 重组语义：回滚旧分支到分叉点（逐笔 Revert），再按顺序应用新分支，
// 结果与从创世按新主链顺序全量重放完全一致。
//
// Chain 不做并发控制，并发安全由 Node 保证。
type Chain struct {
	genesis *Block              // 创世区块
	alloc   map[string]uint64   // 创世余额分配
	blocks  map[[32]byte]*Block // 所有已导入区块（含侧链）
	score   map[[32]byte]uint64 // 从创世到该区块的累积权重
	head    *Block              // 当前主链顶端
	state   *State              // 主链顶端对应的账本状态
}

// NewChain 创建以创世区块为根的链。genesis 高度必须为 0。
func NewChain(genesis *Block, genesisState map[string]uint64) *Chain {
	if genesis.Weight == 0 {
		genesis.Weight = 1
	}
	if genesis.BlockHash == ([32]byte{}) {
		genesis.Seal()
	}
	alloc := make(map[string]uint64, len(genesisState))
	for k, v := range genesisState {
		alloc[k] = v
	}
	c := &Chain{
		genesis: genesis,
		alloc:   alloc,
		blocks:  map[[32]byte]*Block{genesis.BlockHash: genesis},
		score:   map[[32]byte]uint64{genesis.BlockHash: genesis.Weight},
		head:    genesis,
		state:   NewState(genesisState),
	}
	for _, tx := range genesis.Txs {
		_ = c.state.Apply(tx)
	}
	return c
}

// Import 校验并导入一个区块，必要时触发重组。
// 错误可区分：ErrTamperedHash / ErrDuplicateBlock / ErrUnknownParent /
// ErrBadHeight / ErrInvalidNonce / ErrInsufficientBalance / ErrNegativeAmount。
// 校验失败或仅构成低权重侧链时，已有链状态不受任何影响。
func (c *Chain) Import(b *Block) error {
	// 1. 内容完整性：哈希必须匹配内容，防篡改。
	if b.BlockHash != b.ComputeHash() {
		return ErrTamperedHash
	}
	// 2. 重复导入。
	if _, ok := c.blocks[b.BlockHash]; ok {
		return ErrDuplicateBlock
	}
	// 3. 父区块必须已知（乱序到达的孤儿块被拒绝，不影响现有状态）。
	parent, ok := c.blocks[b.Parent]
	if !ok {
		return ErrUnknownParent
	}
	// 4. 高度必须自洽。
	if b.Height != parent.Height+1 {
		return ErrBadHeight
	}
	if b.Weight == 0 {
		b.Weight = 1
	}
	// 5. 交易合法性：沿该区块自身的祖先链重放到父区块，再逐笔应用，
	//    保证任何被接受的区块在其分支上都是可执行的。
	st := c.replayTo(parent)
	for _, tx := range b.Txs {
		if err := st.Apply(tx); err != nil {
			return fmt.Errorf("block %x tx %s: %w", b.BlockHash[:6], tx, err)
		}
	}

	c.blocks[b.BlockHash] = b
	c.score[b.BlockHash] = c.score[b.Parent] + b.Weight

	// 6. 分叉选择：新块若优于当前主链顶端则重组。
	if betterThan(b, c.score[b.BlockHash], c.head, c.score[c.head.BlockHash]) {
		c.reorgTo(b)
	}
	return nil
}

// betterThan 实现分叉选择规则：先比累积权重，再比哈希字典序。
func betterThan(a *Block, scoreA uint64, b *Block, scoreB uint64) bool {
	if scoreA != scoreB {
		return scoreA > scoreB
	}
	return bytes.Compare(a.BlockHash[:], b.BlockHash[:]) < 0
}

// replayTo 从创世重放到指定区块（含），返回该处的状态副本。
func (c *Chain) replayTo(tip *Block) *State {
	var path []*Block
	for b := tip; ; b = c.blocks[b.Parent] {
		path = append(path, b)
		if b.Height == 0 {
			break
		}
	}
	st := c.stateAtGenesis()
	for i := len(path) - 1; i >= 0; i-- {
		for _, tx := range path[i].Txs {
			_ = st.Apply(tx) // 已入库的区块均已验证过，不会失败
		}
	}
	return st
}

// stateAtGenesis 返回创世状态（创世分配 + 创世区块交易）。
func (c *Chain) stateAtGenesis() *State {
	st := NewState(c.alloc)
	for _, tx := range c.genesis.Txs {
		_ = st.Apply(tx)
	}
	return st
}

// reorgTo 把主链切换到 newTip：先回滚旧分支到分叉点，再按序应用新分支。
func (c *Chain) reorgTo(newTip *Block) {
	var oldPath, newPath []*Block
	a, b := c.head, newTip
	for a.Height > b.Height {
		oldPath = append(oldPath, a)
		a = c.blocks[a.Parent]
	}
	for b.Height > a.Height {
		newPath = append(newPath, b)
		b = c.blocks[b.Parent]
	}
	for a != b {
		oldPath = append(oldPath, a)
		newPath = append(newPath, b)
		a = c.blocks[a.Parent]
		b = c.blocks[b.Parent]
	}
	// 撤销旧分支改动（逆序逐笔 Revert）。
	for _, blk := range oldPath {
		for i := len(blk.Txs) - 1; i >= 0; i-- {
			c.state.Revert(blk.Txs[i])
		}
	}
	// 按顺序重新应用新分支交易。
	for i := len(newPath) - 1; i >= 0; i-- {
		for _, tx := range newPath[i].Txs {
			if err := c.state.Apply(tx); err != nil {
				// 区块入库前已按祖先链验证，此处不应失败。
				panic(fmt.Sprintf("reorg apply failed: %v", err))
			}
		}
	}
	c.head = newTip
}

// Head 返回当前主链顶端区块。
func (c *Chain) Head() *Block { return c.head }

// State 返回当前主链对应的账本状态快照（深拷贝，外部修改不影响链）。
func (c *Chain) State() *State { return c.state.Clone() }

// MainChain 返回从创世到主链顶端的区块序列。
func (c *Chain) MainChain() []*Block {
	var path []*Block
	for b := c.head; ; b = c.blocks[b.Parent] {
		path = append(path, b)
		if b.Height == 0 {
			break
		}
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// Has 判断某哈希的区块是否已导入。
func (c *Chain) Has(hash [32]byte) bool {
	_, ok := c.blocks[hash]
	return ok
}
