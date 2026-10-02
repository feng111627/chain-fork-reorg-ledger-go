# chain-fork-reorg-ledger-go

本地可运行的链式账本节点（Go 实现）。用内存账本模拟账户余额与转账交易，
保证在**分叉、重组与并发交易**下状态结果始终一致且可解释。

## 结构

```
ledger/           核心库
  types.go        Tx / Block 类型与内容哈希（SHA-256）、哨兵错误
  state.go        账本状态：余额 + nonce，Apply / Revert
  chain.go        区块导入校验、分叉选择、链重组、主链差异（ReorgDiff）
  txpool.go       交易池：nonce/余额校验、容量上限、重组后对账（Reconcile）
  node.go         并发安全的节点门面（RWMutex）
cmd/node-demo/    本地演示程序
```

## 区块导入校验

`Chain.Import` 按顺序检查，失败原因可互相区分（均为可 `errors.Is` 判断的哨兵错误）：

| 检查 | 错误 |
|---|---|
| 区块哈希与内容不匹配（内容被篡改） | `ErrTamperedHash` |
| 同一区块重复导入 | `ErrDuplicateBlock` |
| 父区块未知（乱序到达的孤儿块） | `ErrUnknownParent` |
| 高度 ≠ 父高度 + 1 | `ErrBadHeight` |
| 块内交易 nonce 不连续 | `ErrInvalidNonce` |
| 块内交易余额不足 | `ErrInsufficientBalance` |

块内交易的合法性通过「沿该区块自身祖先链从创世重放到父区块，再逐笔应用」来验证，
因此任何被接受的区块在其分支上必然可执行。校验失败或仅构成低权重侧链时，
已有链状态不受任何影响——乱序到达不会打乱链状态。

## 分叉选择规则

统一、确定、可解释的两条规则：

1. **累积权重高者为主链**：区块权重 = 从创世到该区块的 `Weight` 之和（普通区块 Weight=1，
   即最长链；可通过 Weight 模拟工作量差异）。
2. **权重相同，区块哈希字典序小者胜出**：平局时规则确定，所有节点得出相同主链。

## 重组语义

当按上述规则出现更优分支顶端时执行重组（`reorgTo`）：

1. 从旧主链顶端与新分支顶端分别向上回溯，找到**分叉点**；
2. 旧分支区块从顶端到分叉点**逐块、块内逆序逐笔 `Revert`**，撤销全部改动；
3. 新分支区块从分叉点到顶端**按顺序逐笔 `Apply`**，重新生效。

由于 `Revert` 是 `Apply` 的精确逆操作，且所有入库区块都已按其祖先链验证过，
重组后的账户状态与「从创世按新主链顺序全量重放」**逐字节一致**——
测试中用独立的重放实现对此做了断言（`TestReorg` / `TestReorgThenExtend` /
并发测试的最终校验），不会出现余额重复计算或丢失。

## 交易池与并发约束

- **入池校验**（`TxPool.Add`）：nonce 必须恰好等于「链上 nonce + 池中该账户排队数」。
  nonce 偏小是重复（`ErrDuplicateTransaction` / `ErrInvalidNonce`），
  偏大是缺口（`ErrNonceGap`），余额校验把池中排队金额一并计入
  （`ErrInsufficientBalance`）。所有拒绝都返回明确错误，绝不静默丢弃。
- **容量上限**：池子有容量上限（默认 `DefaultPoolCap = 10000`，可用
  `NewNodeWithCap` / `NewTxPoolWithCap` 自定义）。打满后新交易返回
  `ErrPoolFull`（错误信息带当前容量），池子不会无限膨胀；出块或重组腾出
  空间后，新交易以及**此前被拒绝过的同一笔交易**都能正常重新提交入池。
- **打包移除**：`Node.ProduceBlock` 把池中交易按确定顺序（账户字典序 + nonce 升序）
  打包上链，并与导入一样在同一临界区内与池子原子对账。
- **并发模型**：`Node` 的所有公开方法共用一把 `sync.RWMutex`。
  写操作（SubmitTx / ImportBlock / ProduceBlock）互斥且原子——重组与池子对账
  在同一把锁内一次完成，外界看来瞬间结束；读操作（Balance / Nonce / Head /
  Snapshot / Pending）只读到某个完整操作结束后的状态，绝不会读到「重组做了
  一半、池子还没对齐」的中间态。需要同时观察多个账户时请用 `Snapshot()` 获取
  同一时点的完整状态快照，避免逐账户读取跨越写操作。

## 重组后池子始终跟随主链（Reconcile）

`Chain.ImportResult` 在导入区块时除了完成校验与分叉选择，还会返回一个
`ReorgDiff`：

- `Reorged`：本次导入是否触发了重组；
- `Confirmed`：导入后落在主链上、导入前不在主链上的交易（新确认）；
- `Reverted`：导入前在主链上、导入后不在主链上的交易（被撤销）。

`Node` 在导入/出块的同一临界区内调用 `TxPool.Reconcile(state, confirmed, reverted)`，
让池子在任何时刻都与新主链顶端对齐：

1. **已在新主链落账**的交易从待打包集合移除；
2. **nonce 对不上、出缺口**的（含缺口之后的同账户交易）一并清除；
3. **新状态下余额不足**的交易清除（在第一笔不合法处停止，其后全部清除）；
4. **与新主链冲突**的交易（同一账户同一 nonce 在新链上落账成了另一笔）清除；
5. **被重组撤销**的交易（旧主链确认过、新主链没有的）自动回流待打包集合，
   按账户与 nonce 顺序重新排好，后续出块可重新打包。

凡是被清除的交易，其「曾经提交过」的去重痕迹也一并删除——因此一笔交易只要
重新变得合法，再次 `SubmitTx` 就能正常入池，不会被永久当成重复。
最终保证：**不丢交易、不重复计账**（被撤销交易重新打包后在最终主链上恰好出现
一次）。

### 容量与重组的关系

- 容量上限只约束外部**新提交**的交易；
- 重组**撤销回流**的是已经上过链的交易，属于链上事实，回流时不受容量限制，
  以「不丢交易」为优先（回流数量受被撤销区块大小约束，是有界的）；
- 回流后池子可能暂时高于容量上限，此时新提交仍会被 `ErrPoolFull` 拒绝，
  直到出块把空间腾出来。

### 兼容性说明

- `Chain.Import`、`TxPool.Add/Remove/Prune/Pending`、`NewNode` / `NewTxPool`
  等既有签名与默认行为保持不变；新增的是 `Chain.ImportResult`、`ReorgDiff`、
  `TxPool.Reconcile`、`NewNodeWithCap` / `NewTxPoolWithCap`、`Node.Pending`。
- 行为修正：过去 `ImportBlock` 会无条件按新区块交易清池，导致**仅构成侧链的
  区块也会误删池内交易**；现在侧链导入主链不变、池子完全不动，只有真正落到
  主链上的交易才参与对账。这与「池随主链」的既有设计意图一致。
- 未自定义容量时使用默认上限，既有调用方无需改动。

## 本地验证

```bash
# 运行全部单测（含竞争检测），日志可见各业务用例与结果
go test ./... -race -v

# 多轮重复运行，确认并发用例稳定、无 flake
go test ./... -race -count=5

# 只看本次新增的「重组 × 池子」场景
go test ./ledger/ -race -v -run 'Reconcile|ReorgPool|Reverted|PoolCap|ConcurrentReorg'

# 运行演示程序：出块 → 分叉重组 → 非法区块拒绝 → 交易池校验
go run ./cmd/node-demo
```

测试覆盖：

| 业务 | 用例 |
|---|---|
| 导入校验 | `TestImportValidation`（篡改/父缺失/高度错乱/余额不足/nonce 错乱/重复导入） |
| 乱序导入 | `TestOutOfOrderImport` |
| 重组 | `TestReorg`、`TestReorgThenExtend`（重组后状态与全量重放比对） |
| 分叉选择 | `TestForkChoiceTieBreak`（权重平局按哈希决胜） |
| 交易池校验 | `TestPoolAddValidation`、`TestPoolRemoveAndPrune`、`TestPoolPendingOrder` |
| 出块与清池 | `TestProduceBlock`、`TestImportBlockPrunesPool` |
| 撤销交易回流（池） | `TestPoolReconcileReverted`（按 nonce 回流、同 nonce 冲突确定性去重） |
| 重组后失效剔除（池） | `TestPoolReconcileInvalidation`（已落账/nonce 缺口/余额不足/同 nonce 冲突剔除，重新提交不被旧去重挡住） |
| 容量上限（池） | `TestPoolCap`（打满拒绝、腾出恢复、撤销回流不受容量限） |
| 重组 × 池子交叉（节点） | `TestNodeReorgPoolSync`（重组即对齐、冲突交易剔除、状态=重放） |
| 撤销交易重新打包 | `TestNodeRevertedTxRepacked`（回流 → 出块重打包，最终不丢不重） |
| 容量流程（节点） | `TestNodePoolCapFlow`（打满拒绝原因、出块腾位、被拒交易重提成功） |
| 并发（原有） | `TestConcurrentSubmitProduceQuery`（8 账户并发提交 + 后台出块 + 并发快照读） |
| 并发 × 重组（新增） | `TestConcurrentReorgPoolConsistency`（并发提交 + 出块 + 持续分叉重组 + 快照读，`-race` 下验证总量守恒、无中间态、最终状态=重放、每笔交易恰好确认一次） |
