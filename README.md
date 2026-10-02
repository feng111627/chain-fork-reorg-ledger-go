# chain-fork-reorg-ledger-go

本地可运行的链式账本节点（Go 实现）。用内存账本模拟账户余额与转账交易，
保证在**分叉、重组与并发交易**下状态结果始终一致且可解释。

## 结构

```
ledger/           核心库
  types.go        Tx / Block 类型与内容哈希（SHA-256）
  state.go        账本状态：余额 + nonce，Apply / Revert
  chain.go        区块导入校验、分叉选择、链重组
  txpool.go       交易池：nonce 顺序与余额校验
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
- **容量上限**：池子有容量上限（`NewNode` 默认 `DefaultPoolCapacity`，
  可用 `NewNodeWithPoolCap` 自定义，<= 0 表示不限）。池满时新提交返回
  `ErrPoolFull`（错误信息带当前占用/容量），被拒绝的交易不留任何残留标记，
  空间腾出后重新提交同一笔可以成功。
- **池随主链走**（`TxPool.Reorg`，由 `Node.ImportBlock` 在同一临界区内自动调用）：
  主链顶端一切换，池子立即按新主链状态重对齐——
  - 被重组撤销的交易（旧主链确认过、新主链没有）**自动回池**，按账户 + nonce
    重新排好，后续出块会重新打包，不丢交易、不重复计账；与池中已有交易同
    nonce 冲突时以链上确认过的版本为准。回收不占用容量上限（宁可暂时超限
    也不丢已确认交易），超限期间新提交仍被拒绝；
  - 已在新主链落账的、nonce 出缺口的、余额不足的、与新主链冲突的交易
    **立即清除**，并同步清除去重标记——之后同一笔交易重新变合法时再提交
    能正常入池，不会被「之前提交过」挡在门外。
- **打包移除**：`Node.ProduceBlock` 把池中交易按确定顺序（账户字典序 + nonce 升序）
  打包上链并立即从池中移除。
- **并发模型**：`Node` 的所有公开方法共用一把 `sync.RWMutex`。
  写操作（SubmitTx / ImportBlock / ProduceBlock）互斥且原子——重组与池子
  重对齐在同一个临界区内完成，外界永远看不到「链已切换、池还是旧的」的
  中间状态；读操作（Balance / Nonce / Head / Snapshot / PoolPending）
  只读到某个完整操作结束后的状态。
  需要同时观察多个账户时请用 `Snapshot()` 获取同一时点的完整状态快照，
  避免逐账户读取跨越写操作。

### 兼容范围

- `Chain` 的导入校验、分叉选择与重组语义完全不变，既有用例无回归。
- `NewNode` 行为变化仅一点：池子从「不限容量」变为默认 1024 上限；
  需要旧行为可用 `NewNodeWithPoolCap(alloc, 0)`（不限容量）。
- `NewTxPool` 签名增加容量参数（`NewTxPool(stateFn, capacity)`）。
- 新增导出项：`ErrPoolFull`、`DefaultPoolCapacity`、`NewNodeWithPoolCap`、
  `TxPool.Reorg`、`Node.PoolPending`。

## 本地验证

```bash
# 运行全部单测（含竞争检测），日志可见各业务用例与结果
go test ./... -race -v

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
| 交易池 | `TestPoolAddValidation`、`TestPoolRemoveAndPrune`、`TestPoolPendingOrder` |
| 出块与清池 | `TestProduceBlock`、`TestImportBlockPrunesPool` |
| 重组 × 池 | `TestReorgPoolRealign`（被撤销交易自动回池重排并重新打包；失效交易清除、去重标记不残留、重新变合法后可再提交） |
| 池容量 | `TestPoolCapacity`（打满明确拒绝 `ErrPoolFull`、腾出空间后恢复、重组回收不受容量限制不丢交易） |
| 并发 | `TestConcurrentSubmitProduceQuery`（并发提交 + 出块 + 快照读）、`TestConcurrentReorgPoolConsistency`（并发提交/重组/出块/查询，`-race` 下验证快照总量守恒、无失效交易出块、最终不丢不重且状态=重放结果） |

新增场景单独验证：

```bash
go test ./ledger/ -run 'TestReorgPoolRealign|TestPoolCapacity|TestConcurrentReorgPoolConsistency' -race -v
```
