// Package chain 维护区块树，负责区块校验、分叉选择与链重组。
//
// 分叉选择规则（确定且可解释）：
//  1. 每个已导入且有效区块都是区块树上的一个节点，主链为从某个末端
//     （tip，没有子块的节点）沿父指针回溯到创世块的一条路径；
//  2. 比较各候选路径的累积权重（路径上每个区块 Weight 之和，创世块不计），
//     累积权重最大者成为主链；
//  3. 累积权重相同时，末端区块哈希按字节序较小者胜出，
//     保证在任意到达顺序下选择结果唯一且可复现。
//
// 重组语义：切换主链时不做增量余额补丁，而是回到分叉共同祖先，
// 先把旧分叉上的交易全部“撤销”（直接丢弃旧顶端状态），再从共同祖先
// 状态开始按顺序重放新分叉的交易。实现上等价于“从创世块按新主链
// 顺序完整重放”，因此不会出现余额重复计算或丢失。
package chain

import (
	"fmt"

	"github.com/feng111627/chain-fork-reorg-ledger-go/state"
	"github.com/feng111627/chain-fork-reorg-ledger-go/types"
)

// ReorgError 描述一次区块导入被拒绝的业务原因。
type ReorgError struct {
	Reason string
}

func (e *ReorgError) Error() string { return e.Reason }

// Is 支持 errors.Is(err, ErrDuplicate) 这样的判别。
func (e *ReorgError) Is(target error) bool {
	t, ok := target.(*ReorgError)
	if !ok {
		return false
	}
	return e.Reason == t.Reason
}

// 互相可区分的拒绝原因（错误信息与哨兵实例均不同）。
var (
	ErrDuplicate      = &ReorgError{Reason: "duplicate block: block already imported"}
	ErrParentMissing  = &ReorgError{Reason: "parent missing: unknown parent hash"}
	ErrHeightMismatch = &ReorgError{Reason: "height mismatch: block height != parent height + 1"}
	ErrTampered       = &ReorgError{Reason: "tampered block: block hash mismatch (content does not match declared hash)"}
	ErrInvalidTx      = &ReorgError{Reason: "invalid transaction in block"}
	ErrBadGenesis     = &ReorgError{Reason: "bad genesis: height must be 0 and parent must be zero hash"}
	ErrGenesisExists  = &ReorgError{Reason: "genesis already exists"}
	ErrUnknownParent  = &ReorgError{Reason: "unknown parent"}
)

// blockNode 区块树节点。
//
// 每个节点持有在父状态上应用本块交易后派生出的状态快照，
// 因此切换到任意分叉末端都能立即得到该分叉的确定状态；
// 状态是深拷贝派生的，分叉之间互不污染。
type blockNode struct {
	block  types.Block
	hash   types.Hash
	state  *state.State
	parent *blockNode
}

// Chain 区块树与当前主链。非并发安全，由 node.Node 持锁访问。
type Chain struct {
	genesis *blockNode
	nodes   map[types.Hash]*blockNode
	tip     *blockNode
}

// New 以创世区块初始化链。创世块必须高度为 0 且父哈希为零哈希。
// 创世块的交易用于初始分配（通常每笔交易从一个特殊创世地址转出，
// 或由调用方保证状态自洽），与普通块一样按顺序执行。
func New(genesis types.Block) (*Chain, error) {
	if genesis.Height != 0 || genesis.ParentHash != types.ZeroHash {
		return nil, ErrBadGenesis
	}
	gh := genesis.Hash()
	st := state.New()
	for i, tx := range genesis.Txs {
		if err := st.Apply(tx); err != nil {
			return nil, fmt.Errorf("%w: genesis tx #%d: %v", ErrInvalidTx, i, err)
		}
	}
	root := &blockNode{block: genesis, hash: gh, state: st}
	c := &Chain{
		genesis: root,
		nodes:   map[types.Hash]*blockNode{gh: root},
		tip:     root,
	}
	return c, nil
}

// NewWithGenesisState 允许调用方直接提供创世状态（初始余额分配），
// 创世块本身可以不含交易。
func NewWithGenesisState(genesis types.Block, genesisState *state.State) (*Chain, error) {
	if genesis.Height != 0 || genesis.ParentHash != types.ZeroHash {
		return nil, ErrBadGenesis
	}
	gh := genesis.Hash()
	root := &blockNode{block: genesis, hash: gh, state: genesisState.Clone()}
	c := &Chain{
		genesis: root,
		nodes:   map[types.Hash]*blockNode{gh: root},
		tip:     root,
	}
	return c, nil
}

// Import 导入一个区块（本地场景：信任区块内容自算哈希）并执行严格校验：
//   - 重复导入（哈希已存在）→ ErrDuplicate；
//   - 父区块未知（乱序提前到达）→ ErrParentMissing，链状态不变；
//   - 高度不等于父高度+1 → ErrHeightMismatch；
//   - 块内交易不合法（nonce/余额）→ ErrInvalidTx，链状态不变。
//
// 网络场景请使用 ImportAnnounced：对端会随区块附上声明哈希，
// 内容被篡改（重算哈希 != 声明哈希）将得到 ErrTampered。
//
// 校验全部通过后区块才挂入树中，随后重算分叉选择；
// 若新区块所在分叉成为主链，则执行重组。返回值 switched
// 表示本次导入是否触发了主链切换。
func (c *Chain) Import(b types.Block) (switched bool, err error) {
	return c.importWith(b, b.Hash())
}

// ImportAnnounced 导入一个带“声明哈希”的区块公告（模拟网络消息）。
// 接收方先重算区块哈希并与公告中的声明哈希比对，不一致即判定内容被篡改，
// 返回 ErrTampered，与其它拒绝原因明确区分；之后执行与 Import 相同的校验。
func (c *Chain) ImportAnnounced(ann types.Announce) (switched bool, err error) {
	got := ann.Block.Hash()
	if got != ann.Hash {
		return false, fmt.Errorf("%w: announced %s but recomputed %s",
			ErrTampered, ann.Hash.Hex(), got.Hex())
	}
	return c.importWith(ann.Block, got)
}

func (c *Chain) importWith(b types.Block, hash types.Hash) (switched bool, err error) {

	// 1. 重复导入：直接拒绝，保证导入操作幂等可区分。
	if c.HasBlock(hash) {
		return false, ErrDuplicate
	}

	// 2. 创世相关：非零高度却引用零父哈希、或重复创世。
	if b.ParentHash == types.ZeroHash {
		return false, ErrBadGenesis
	}

	// 3. 父区块必须存在。乱序到达（父还没到）时在此被明确拒绝，
	//    不会在链中留下任何半成品节点。
	parent, ok := c.nodes[b.ParentHash]
	if !ok {
		return false, fmt.Errorf("%w: block at height %d references parent %s",
			ErrParentMissing, b.Height, b.ParentHash.Hex())
	}

	// 4. 高度自洽：height == parent.height + 1。
	if b.Height != parent.block.Height+1 {
		return false, fmt.Errorf("%w: block height %d, parent height %d",
			ErrHeightMismatch, b.Height, parent.block.Height)
	}

	// 5. 在父分叉状态的副本上试执行全部交易；
	//    任一枚交易非法则整块拒绝，已有链状态完全不受影响。
	st := parent.state.Clone()
	for i, tx := range b.Txs {
		if err := st.Apply(tx); err != nil {
			return false, fmt.Errorf("%w: height %d tx #%d (%s->%s nonce %d): %v",
				ErrInvalidTx, b.Height, i, tx.From, tx.To, tx.Nonce, err)
		}
	}

	// 全部通过：正式挂树（注意 st 只在此刻才成为树的一部分）。
	node := &blockNode{block: b, hash: hash, state: st, parent: parent}
	c.nodes[hash] = node

	// 6. 重算分叉选择。
	newTip := c.chooseHead()
	if newTip != c.tip {
		c.tip = newTip
		return true, nil
	}
	return false, nil
}

// chooseHead 枚举全部末端节点，按“累积权重最大、平手取末端哈希字节序较小”
// 的统一规则选出主链末端。
func (c *Chain) chooseHead() *blockNode {
	var best *blockNode
	var bestWeight uint64
	for _, n := range c.nodes {
		if c.hasChild(n) {
			continue
		}
		w := n.cumulativeWeight()
		switch {
		case best == nil:
			best, bestWeight = n, w
		case w > bestWeight:
			best, bestWeight = n, w
		case w == bestWeight && compareHash(n.hash, best.hash) < 0:
			best, bestWeight = n, w
		}
	}
	return best
}

// hasChild 判断节点是否被其他节点引用为父。
func (c *Chain) hasChild(n *blockNode) bool {
	for _, m := range c.nodes {
		if m.parent == n {
			return true
		}
	}
	return false
}

func compareHash(a, b types.Hash) int {
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// pathToGenesis 返回节点沿父指针到创世块的节点序列（创世在前）。
func (n *blockNode) pathToGenesis() []*blockNode {
	var path []*blockNode
	for cur := n; cur != nil; cur = cur.parent {
		path = append(path, cur)
	}
	// 反转，使创世块在最前。
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// cumulativeWeight 返回节点回溯路径上的权重之和（含自身，含创世）。
func (n *blockNode) cumulativeWeight() uint64 {
	var sum uint64
	for cur := n; cur != nil; cur = cur.parent {
		sum += cur.block.Weight
	}
	return sum
}

// Tip 返回当前主链末端区块。
func (c *Chain) Tip() types.Block { return c.tip.block }

// TipHash 返回当前主链末端哈希。
func (c *Chain) TipHash() types.Hash { return c.tip.hash }

// Height 返回当前主链高度。
func (c *Chain) Height() uint64 { return c.tip.block.Height }

// TipWeight 返回当前主链的累积权重（解释/日志用）。
func (c *Chain) TipWeight() uint64 { return c.tip.cumulativeWeight() }

// State 返回当前主链顶端状态的深拷贝，调用方可任意修改而不影响链。
func (c *Chain) State() *state.State { return c.tip.state.Clone() }

// Canonical 返回主链从创世到顶端的区块序列（创世在前）。
func (c *Chain) Canonical() []types.Block {
	path := c.tip.pathToGenesis()
	out := make([]types.Block, len(path))
	for i, n := range path {
		out[i] = n.block
	}
	return out
}

// CanonicalHashes 返回主链哈希摘要序列（创世在前）。
func (c *Chain) CanonicalHashes() []types.Hash {
	path := c.tip.pathToGenesis()
	out := make([]types.Hash, len(path))
	for i, n := range path {
		out[i] = n.hash
	}
	return out
}

// ReplayFromGenesis 从创世状态开始按当前主链顺序完整重放所有交易，
// 返回重放得到的状态。用于与顶端缓存状态做一致性核对
// （重组正确性的最终判据）。
func (c *Chain) ReplayFromGenesis() *state.State {
	st := c.genesis.state.Clone()
	path := c.tip.pathToGenesis()
	for _, n := range path[1:] { // 跳过创世块（其状态即初始状态）
		for _, tx := range n.block.Txs {
			if err := st.Apply(tx); err != nil {
				// 树上的块导入时已校验通过，此处不应发生。
				panic(fmt.Errorf("unexpected invalid tx during replay: %w", err))
			}
		}
	}
	return st
}

// HasBlock 判断区块（按哈希）是否已导入。
func (c *Chain) HasBlock(h types.Hash) bool {
	_, ok := c.nodes[h]
	return ok
}

// Block 按哈希取回已导入区块，第二个返回值表示是否存在。
func (c *Chain) Block(h types.Hash) (types.Block, bool) {
	n, ok := c.nodes[h]
	if !ok {
		return types.Block{}, false
	}
	return n.block, true
}

// VerifyTipState 核对当前主链顶端缓存状态与“从创世块按主链顺序完整重放”
// 得到的状态是否完全一致。重组正确性的最终判据：返回 true 表示
// 没有余额重复计算或丢失；返回 false 说明状态机存在缺陷。
func (c *Chain) VerifyTipState() bool {
	return c.tip.state.Equal(c.ReplayFromGenesis())
}
