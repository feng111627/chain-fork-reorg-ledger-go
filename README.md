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
- **打包移除**：`Node.ProduceBlock` 把池中交易按确定顺序（账户字典序 + nonce 升序）
  打包上链并立即从池中移除；`ImportBlock` 也会清除已上链（含重组后新主链确认）的交易。
- **并发模型**：`Node` 的所有公开方法共用一把 `sync.RWMutex`。
  写操作（SubmitTx / ImportBlock / ProduceBlock）互斥且原子——重组在外界看来瞬间完成；
  读操作（Balance / Nonce / Head / Snapshot）只读到某个完整操作结束后的状态。
  需要同时观察多个账户时请用 `Snapshot()` 获取同一时点的完整状态快照，
  避免逐账户读取跨越写操作。

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
| 并发 | `TestConcurrentSubmitProduceQuery`（8 账户并发提交 + 后台出块 + 并发快照读，`-race` 下验证总量守恒与最终状态=重放结果） |
