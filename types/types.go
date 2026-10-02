// Package types 定义链式账本的核心数据类型：地址、交易与区块。
package types

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// Hash 区块/交易摘要，使用 SHA-256。
type Hash [32]byte

// Hex 返回哈希前 8 字节的十六进制形式，便于日志展示。
func (h Hash) Hex() string { return hex.EncodeToString(h[:8]) }

// Address 账户地址。
type Address string

// Transaction 表示一笔从 From 到 To 的转账。
//
// Nonce 是发送方账户的单调递增序号（首笔交易为 0），
// 用于防止重放并确定同一发送方多笔交易的执行顺序。
type Transaction struct {
	From  Address
	To    Address
	Nonce uint64 // 发送方账户的单调递增序号，首笔为 0
	Value uint64 // 转账金额（最小单位）
}

// Hash 返回交易内容的 SHA-256 摘要。
func (tx Transaction) Hash() Hash {
	h := sha256.New()
	h.Write([]byte(tx.From))
	h.Write([]byte{0})
	h.Write([]byte(tx.To))
	h.Write([]byte{0})
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], tx.Nonce)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], tx.Value)
	h.Write(buf[:])
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// Block 表示链上的一个区块。
//
// Weight 是区块权重（工作量证明的抽象）：分叉选择时比较
// 各候选路径上区块 Weight 的累积和，而不是只比较区块高度。
type Block struct {
	ParentHash Hash          // 父区块哈希，创世块为零哈希
	Height     uint64        // 区块高度，创世块为 0
	Weight     uint64        // 区块权重（工作量证明的抽象）
	Txs        []Transaction // 区块内按顺序执行的交易
}

// Hash 返回区块全部字段的 SHA-256 摘要。
// 任何字段（包括交易内容与顺序）被篡改都会导致哈希变化，
// 因此父区块中保存的 ParentHash 可形成防篡改的哈希链。
func (b Block) Hash() Hash {
	h := sha256.New()
	h.Write(b.ParentHash[:])
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], b.Height)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], b.Weight)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(len(b.Txs)))
	h.Write(buf[:])
	for i := range b.Txs {
		txh := b.Txs[i].Hash()
		h.Write(txh[:])
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// ZeroHash 全零哈希，用作创世块的父哈希。
var ZeroHash Hash

// Announce 模拟网络上传播的区块消息：区块体之外额外携带发送方声明的哈希。
//
// 接收方重算 Block.Hash() 并与 Announce.Hash 比对：
// 任何传输/存储环节对区块内容的篡改都会使两者不一致，
// 从而在导入时被明确识别为“内容篡改”，而不是被当成一个普通新区块。
type Announce struct {
	Block Block
	Hash  Hash // 发送方声明的区块哈希
}
