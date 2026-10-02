// Package ledger 实现一个本地可运行的链式账本节点，
// 支持区块导入校验、分叉选择、链重组与并发交易处理。
package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// 区块导入与交易校验的错误原因，调用方可据此区分失败类别。
var (
	ErrUnknownParent        = errors.New("unknown parent block")
	ErrBadHeight            = errors.New("block height does not equal parent height + 1")
	ErrTamperedHash         = errors.New("block hash mismatch: content tampered")
	ErrDuplicateBlock       = errors.New("block already imported")
	ErrInvalidNonce         = errors.New("transaction nonce mismatch")
	ErrInsufficientBalance  = errors.New("insufficient balance")
	ErrNonceGap             = errors.New("transaction nonce gap")
	ErrDuplicateTransaction = errors.New("transaction already in pool")
	ErrNegativeAmount       = errors.New("transaction amount must be positive")
)

// Tx 是一笔转账交易。
type Tx struct {
	From   string
	To     string
	Amount uint64
	Nonce  uint64 // 发送方账户的第 N 笔交易，从 0 开始递增
}

// Hash 返回交易内容的哈希，用于区块哈希计算与交易去重。
func (tx Tx) Hash() [32]byte {
	h := sha256.New()
	h.Write([]byte(tx.From))
	h.Write([]byte{0})
	h.Write([]byte(tx.To))
	h.Write([]byte{0})
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], tx.Amount)
	binary.BigEndian.PutUint64(buf[8:16], tx.Nonce)
	h.Write(buf[:])
	return sha256.Sum256(h.Sum(nil))
}

func (tx Tx) String() string {
	return fmt.Sprintf("%s->%s amt=%d nonce=%d", tx.From, tx.To, tx.Amount, tx.Nonce)
}

// Block 是一个账本区块。Weight 参与分叉选择，普通区块为 1。
type Block struct {
	Height    uint64
	Parent    [32]byte
	Txs       []Tx
	Weight    uint64
	BlockHash [32]byte
}

// ComputeHash 根据区块内容计算哈希（不含 BlockHash 字段本身）。
func (b *Block) ComputeHash() [32]byte {
	h := sha256.New()
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], b.Height)
	binary.BigEndian.PutUint64(buf[8:16], b.Weight)
	h.Write(buf[:])
	h.Write(b.Parent[:])
	for _, tx := range b.Txs {
		th := tx.Hash()
		h.Write(th[:])
	}
	return sha256.Sum256(h.Sum(nil))
}

// Seal 计算并填入 BlockHash。
func (b *Block) Seal() {
	b.BlockHash = b.ComputeHash()
}
