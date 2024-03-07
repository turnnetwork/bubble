// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/syndtr/goleveldb/leveldb/iterator"

	"github.com/bubblenet/bubble/common"
	"github.com/bubblenet/bubble/core/snapshotdb"
	"github.com/bubblenet/bubble/core/types"
	"github.com/bubblenet/bubble/log"
	"github.com/bubblenet/bubble/rlp"
	"github.com/bubblenet/bubble/trie"
)

// handleGetBlockHeaders handles Block header query, collect the requested headers and reply
func handleGetBlockHeaders(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the complex header query
	var query GetBlockHeadersPacket
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetBlockHeadersQuery(backend, &query, peer)
	return peer.SendBlockHeaders(response)
}

// handleGetBlockHeaders66 is the eth/66 version of handleGetBlockHeaders
func handleGetBlockHeaders66(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the complex header query
	var query GetBlockHeadersPacket66
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetBlockHeadersQuery(backend, query.GetBlockHeadersPacket, peer)
	return peer.ReplyBlockHeaders(query.RequestId, response)
}

func answerGetBlockHeadersQuery(backend Backend, query *GetBlockHeadersPacket, peer *Peer) []*types.Header {
	hashMode := query.Origin.Hash != (common.Hash{})
	first := true
	maxNonCanonical := uint64(100)

	// Gather headers until the fetch or network limits is reached
	var (
		bytes   common.StorageSize
		headers []*types.Header
		unknown bool
		lookups int
	)
	for !unknown && len(headers) < int(query.Amount) && bytes < softResponseLimit &&
		len(headers) < maxHeadersServe && lookups < 2*maxHeadersServe {
		lookups++
		// Retrieve the next header satisfying the query
		var origin *types.Header
		if hashMode {
			if first {
				first = false
				origin = backend.Chain().GetHeaderByHash(query.Origin.Hash)
				if origin != nil {
					query.Origin.Number = origin.Number.Uint64()
				}
			} else {
				origin = backend.Chain().GetHeader(query.Origin.Hash, query.Origin.Number)
			}
		} else {
			origin = backend.Chain().GetHeaderByNumber(query.Origin.Number)
		}
		if origin == nil {
			break
		}
		headers = append(headers, origin)
		bytes += estHeaderSize

		// Advance to the next header of the query
		switch {
		case hashMode && query.Reverse:
			// Hash based traversal towards the genesis block
			ancestor := query.Skip + 1
			if ancestor == 0 {
				unknown = true
			} else {
				query.Origin.Hash, query.Origin.Number = backend.Chain().GetAncestor(query.Origin.Hash, query.Origin.Number, ancestor, &maxNonCanonical)
				unknown = (query.Origin.Hash == common.Hash{})
			}
		case hashMode && !query.Reverse:
			// Hash based traversal towards the leaf block
			var (
				current = origin.Number.Uint64()
				next    = current + query.Skip + 1
			)
			if next <= current {
				infos, _ := json.MarshalIndent(peer.Peer.Info(), "", "  ")
				peer.Log().Warn("GetBlockHeaders skip overflow attack", "current", current, "skip", query.Skip, "next", next, "attacker", infos)
				unknown = true
			} else {
				if header := backend.Chain().GetHeaderByNumber(next); header != nil {
					nextHash := header.Hash()
					expOldHash, _ := backend.Chain().GetAncestor(nextHash, next, query.Skip+1, &maxNonCanonical)
					if expOldHash == query.Origin.Hash {
						query.Origin.Hash, query.Origin.Number = nextHash, next
					} else {
						unknown = true
					}
				} else {
					unknown = true
				}
			}
		case query.Reverse:
			// Number based traversal towards the genesis block
			if query.Origin.Number >= query.Skip+1 {
				query.Origin.Number -= query.Skip + 1
			} else {
				unknown = true
			}

		case !query.Reverse:
			// Number based traversal towards the leaf block
			query.Origin.Number += query.Skip + 1
		}
	}
	return headers
}

func handleGetBlockBodies(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the block body retrieval message
	var query GetBlockBodiesPacket
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetBlockBodiesQuery(backend, query, peer)
	return peer.SendBlockBodiesRLP(response)
}

func handleGetBlockBodies66(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the block body retrieval message
	var query GetBlockBodiesPacket66
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetBlockBodiesQuery(backend, query.GetBlockBodiesPacket, peer)
	return peer.ReplyBlockBodiesRLP(query.RequestId, response)
}

func answerGetBlockBodiesQuery(backend Backend, query GetBlockBodiesPacket, peer *Peer) []rlp.RawValue {
	// Gather blocks until the fetch or network limits is reached
	var (
		bytes  int
		bodies []rlp.RawValue
	)
	for lookups, hash := range query {
		if bytes >= softResponseLimit || len(bodies) >= maxBodiesServe ||
			lookups >= 2*maxBodiesServe {
			break
		}
		if data := backend.Chain().GetBodyRLP(hash); len(data) != 0 {
			bodies = append(bodies, data)
			bytes += len(data)
		}
	}
	return bodies
}

func handleGetNodeData(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the trie node data retrieval message
	var query GetNodeDataPacket
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetNodeDataQuery(backend, query, peer)
	return peer.SendNodeData(response)
}

func handleGetNodeData66(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the trie node data retrieval message
	var query GetNodeDataPacket66
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetNodeDataQuery(backend, query.GetNodeDataPacket, peer)
	return peer.ReplyNodeData(query.RequestId, response)
}

func answerGetNodeDataQuery(backend Backend, query GetNodeDataPacket, peer *Peer) [][]byte {
	// Gather state data until the fetch or network limits is reached
	var (
		bytes int
		nodes [][]byte
	)
	for lookups, hash := range query {
		if bytes >= softResponseLimit || len(nodes) >= maxNodeDataServe ||
			lookups >= 2*maxNodeDataServe {
			break
		}
		// Retrieve the requested state entry
		if bloom := backend.StateBloom(); bloom != nil && !bloom.Contains(hash[:]) {
			// Only lookup the trie node if there's chance that we actually have it
			continue
		}
		entry, err := backend.Chain().TrieNode(hash)
		if len(entry) == 0 || err != nil {
			// Read the contract code with prefix only to save unnecessary lookups.
			entry, err = backend.Chain().ContractCodeWithPrefix(hash)
		}
		if err == nil && len(entry) > 0 {
			nodes = append(nodes, entry)
			bytes += len(entry)
		}
	}
	return nodes
}

func handleGetReceipts(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the block receipts retrieval message
	var query GetReceiptsPacket
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetReceiptsQuery(backend, query, peer)
	return peer.SendReceiptsRLP(response)
}

func handleGetReceipts66(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the block receipts retrieval message
	var query GetReceiptsPacket66
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	response := answerGetReceiptsQuery(backend, query.GetReceiptsPacket, peer)
	return peer.ReplyReceiptsRLP(query.RequestId, response)
}

func answerGetReceiptsQuery(backend Backend, query GetReceiptsPacket, peer *Peer) []rlp.RawValue {
	// Gather state data until the fetch or network limits is reached
	var (
		bytes    int
		receipts []rlp.RawValue
	)
	for lookups, hash := range query {
		if bytes >= softResponseLimit || len(receipts) >= maxReceiptsServe ||
			lookups >= 2*maxReceiptsServe {
			break
		}
		// Retrieve the requested block's receipts
		results := backend.Chain().GetReceiptsByHash(hash)
		if results == nil {
			if header := backend.Chain().GetHeaderByHash(hash); header == nil || header.ReceiptHash != types.EmptyRootHash {
				continue
			}
		}
		// If known, encode and queue for response packet
		if encoded, err := rlp.EncodeToBytes(results); err != nil {
			log.Error("Failed to encode receipt", "err", err)
		} else {
			receipts = append(receipts, encoded)
			bytes += len(encoded)
		}
	}
	return receipts
}

func handleNewBlockhashes(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of new block announcements just arrived
	ann := new(NewBlockHashesPacket)
	if err := msg.Decode(ann); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	// Mark the hashes as present at the remote node
	for _, block := range *ann {
		peer.markBlock(block.Hash)
	}
	// Deliver them all to the backend for queuing
	return backend.Handle(peer, ann)
}

func handleNewBlock(backend Backend, msg Decoder, peer *Peer) error {
	// Retrieve and decode the propagated block
	ann := new(NewBlockPacket)
	if err := msg.Decode(ann); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	if err := ann.sanityCheck(); err != nil {
		return err
	}
	if hash := types.DeriveSha(ann.Block.Transactions(), trie.NewStackTrie(nil)); hash != ann.Block.TxHash() {
		log.Warn("Propagated block has invalid body", "have", hash, "exp", ann.Block.TxHash())
		return nil // TODO(karalabe): return error eventually, but wait a few releases
	}
	ann.Block.ReceivedAt = msg.Time()
	ann.Block.ReceivedFrom = peer

	// Mark the peer as owning the block
	peer.markBlock(ann.Block.Hash())

	return backend.Handle(peer, ann)
}

func handleBlockHeaders(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of headers arrived to one of our previous requests
	res := new(BlockHeadersPacket)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, res)
}

func handleBlockHeaders66(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of headers arrived to one of our previous requests
	res := new(BlockHeadersPacket66)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	requestTracker.Fulfil(peer.id, peer.version, BlockHeadersMsg, res.RequestId)

	return backend.Handle(peer, &res.BlockHeadersPacket)
}

func handleBlockBodies(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of block bodies arrived to one of our previous requests
	res := new(BlockBodiesPacket)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, res)
}

func handleBlockBodies66(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of block bodies arrived to one of our previous requests
	res := new(BlockBodiesPacket66)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	requestTracker.Fulfil(peer.id, peer.version, BlockBodiesMsg, res.RequestId)

	return backend.Handle(peer, &res.BlockBodiesPacket)
}

func handleNodeData(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of node state data arrived to one of our previous requests
	res := new(NodeDataPacket)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, res)
}

func handleNodeData66(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of node state data arrived to one of our previous requests
	res := new(NodeDataPacket66)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}

	return backend.Handle(peer, &res.NodeDataPacket)
}

func handleReceipts(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of receipts arrived to one of our previous requests
	res := new(ReceiptsPacket)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, res)
}

func handleReceipts66(backend Backend, msg Decoder, peer *Peer) error {
	// A batch of receipts arrived to one of our previous requests
	res := new(ReceiptsPacket66)
	if err := msg.Decode(res); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	requestTracker.Fulfil(peer.id, peer.version, ReceiptsMsg, res.RequestId)

	return backend.Handle(peer, &res.ReceiptsPacket)
}

func handleNewPooledTransactionHashes(backend Backend, msg Decoder, peer *Peer) error {
	// New transaction announcement arrived, make sure we have
	// a valid and fresh chain to handle them
	if !backend.AcceptTxs() || !backend.AcceptRemoteTxs() {
		return nil
	}
	ann := new(NewPooledTransactionHashesPacket)
	if err := msg.Decode(ann); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	// Schedule all the unknown hashes for retrieval
	for _, hash := range *ann {
		peer.markTransaction(hash)
	}
	return backend.Handle(peer, ann)
}

func handleGetPooledTransactions(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the pooled transactions retrieval message
	var query GetPooledTransactionsPacket
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	hashes, txs := answerGetPooledTransactions(backend, query, peer)
	return peer.SendPooledTransactionsRLP(hashes, txs)
}

func handleGetPooledTransactions66(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the pooled transactions retrieval message
	var query GetPooledTransactionsPacket66
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	hashes, txs := answerGetPooledTransactions(backend, query.GetPooledTransactionsPacket, peer)
	return peer.ReplyPooledTransactionsRLP(query.RequestId, hashes, txs)
}

func answerGetPooledTransactions(backend Backend, query GetPooledTransactionsPacket, peer *Peer) ([]common.Hash, []rlp.RawValue) {
	// Gather transactions until the fetch or network limits is reached
	var (
		bytes  int
		hashes []common.Hash
		txs    []rlp.RawValue
	)
	for _, hash := range query {
		if bytes >= softResponseLimit {
			break
		}
		// Retrieve the requested transaction, skipping if unknown to us
		tx := backend.TxPool().Get(hash)
		if tx == nil {
			continue
		}
		// If known, encode and queue for response packet
		if encoded, err := rlp.EncodeToBytes(tx); err != nil {
			log.Error("Failed to encode transaction", "err", err)
		} else {
			hashes = append(hashes, hash)
			txs = append(txs, encoded)
			bytes += len(encoded)
		}
	}
	return hashes, txs
}

func handleTransactions(backend Backend, msg Decoder, peer *Peer) error {
	// Transactions arrived, make sure we have a valid and fresh chain to handle them
	if !backend.AcceptTxs() || !backend.AcceptRemoteTxs() {
		return nil
	}
	// Transactions can be processed, parse all of them and deliver to the pool
	var txs TransactionsPacket
	if err := msg.Decode(&txs); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	for i, tx := range txs {
		// Validate and mark the remote transaction
		if tx == nil {
			return fmt.Errorf("%w: transaction %d is nil", errDecode, i)
		}
		peer.markTransaction(tx.Hash())
	}
	return backend.Handle(peer, &txs)
}

func handlePooledTransactions(backend Backend, msg Decoder, peer *Peer) error {
	// Transactions arrived, make sure we have a valid and fresh chain to handle them
	if !backend.AcceptTxs() || !backend.AcceptRemoteTxs() {
		return nil
	}
	// Transactions can be processed, parse all of them and deliver to the pool
	var txs PooledTransactionsPacket
	if err := msg.Decode(&txs); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	for i, tx := range txs {
		// Validate and mark the remote transaction
		if tx == nil {
			return fmt.Errorf("%w: transaction %d is nil", errDecode, i)
		}
		peer.markTransaction(tx.Hash())
	}
	return backend.Handle(peer, &txs)
}

func handlePooledTransactions66(backend Backend, msg Decoder, peer *Peer) error {
	// Transactions arrived, make sure we have a valid and fresh chain to handle them
	if !backend.AcceptTxs() || !backend.AcceptRemoteTxs() {
		return nil
	}
	// Transactions can be processed, parse all of them and deliver to the pool
	var txs PooledTransactionsPacket66
	if err := msg.Decode(&txs); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	for i, tx := range txs.PooledTransactionsPacket {
		// Validate and mark the remote transaction
		if tx == nil {
			return fmt.Errorf("%w: transaction %d is nil", errDecode, i)
		}
		peer.markTransaction(tx.Hash())
	}

	return backend.Handle(peer, &txs.PooledTransactionsPacket)
}

// handleGetPPOSStorageMsg handles PPOS Storage query, collect the requested Storage and reply
func handleGetPPOSStorageMsg(backend Backend, msg Decoder, peer *Peer) error {
	var query []interface{}
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}

	_ = answerGetPPOSStorageMsgQuery(backend, peer)
	return nil
}

func answerGetPPOSStorageMsgQuery(backend Backend, peer *Peer) []rlp.RawValue {
	f := func(num *big.Int, iter iterator.Iterator) error {
		var psInfo PposInfoPack
		if num == nil {
			return errors.New("num should not be nil")
		}
		psInfo.Pivot = backend.Chain().GetHeaderByNumber(num.Uint64())
		psInfo.Latest = backend.Chain().CurrentHeader()
		if err := peer.SendPPOSInfo(psInfo); err != nil {
			peer.Log().Error("[GetPPOSStorageMsg]send last ppos meassage fail", "error", err)
			return err
		}
		var (
			byteSize int
			ps       PposStoragePack
			count    int
		)
		ps.KVs = make([][2][]byte, 0)
		for iter.Next() {
			if bytes.Equal(iter.Key(), []byte(snapshotdb.CurrentHighestBlock)) || bytes.Equal(iter.Key(), []byte(snapshotdb.CurrentBaseNum)) || bytes.HasPrefix(iter.Key(), []byte(snapshotdb.WalKeyPrefix)) {
				continue
			}
			byteSize = byteSize + len(iter.Key()) + len(iter.Value())
			if count >= PPOSStorageKVSizeFetch || byteSize > softResponseLimit {
				if err := peer.SendPPOSStorage(ps); err != nil {
					peer.Log().Error("[GetPPOSStorageMsg]send ppos message fail", "error", err, "kvnum", ps.KVNum)
					return err
				}
				count = 0
				ps.KVs = make([][2][]byte, 0)
				byteSize = 0
			}
			k, v := make([]byte, len(iter.Key())), make([]byte, len(iter.Value()))
			copy(k, iter.Key())
			copy(v, iter.Value())
			ps.KVs = append(ps.KVs, [2][]byte{
				k, v,
			})
			ps.KVNum++
			count++
		}
		ps.Last = true
		if err := peer.SendPPOSStorage(ps); err != nil {
			peer.Log().Error("[GetPPOSStorageMsg]send last ppos message fail", "error", err)
			return err
		}
		return nil
	}
	var err error
	go func() {
		if err = snapshotdb.Instance().WalkBaseDB(nil, f); err != nil {
			peer.Log().Error("[GetPPOSStorageMsg]send  ppos storage fail", "error", err)
		}
	}()
	return nil
}

// handlePPosStorageMsg handles PPOS msg, collect the requested info and reply
func handlePPosStorageMsg(backend Backend, msg Decoder, peer *Peer) error {

	peer.Log().Debug("Received a broadcast message[PposStorageMsg]")
	var data PposStoragePack
	if err := msg.Decode(&data); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	// Deliver all to the downloader
	return backend.Handle(peer, &data)
}

func handleGetOriginAndPivotMsg(backend Backend, msg Decoder, peer *Peer) error {

	peer.Log().Info("[GetOriginAndPivotMsg]Received a broadcast message")
	var query uint64
	if err := msg.Decode(&query); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	oHead := backend.Chain().GetHeaderByNumber(query)
	pivot, err := snapshotdb.Instance().BaseNum()
	if err != nil {
		peer.Log().Error("GetOriginAndPivot get snapshotdb baseNum fail", "err", err)
		return errors.New("GetOriginAndPivot get snapshotdb baseNum fail")
	}
	if pivot == nil {
		peer.Log().Error("[GetOriginAndPivot] pivot should not be nil")
		return errors.New("[GetOriginAndPivot] pivot should not be nil")
	}
	pHead := backend.Chain().GetHeaderByNumber(pivot.Uint64())

	data := make([]*types.Header, 0)
	data = append(data, oHead, pHead)
	if err := peer.SendOriginAndPivot(data); err != nil {
		peer.Log().Error("[GetOriginAndPivotMsg]send data meassage fail", "error", err)
		return err
	}
	return nil
}

func handleOriginAndPivotMsg(backend Backend, msg Decoder, peer *Peer) error {
	peer.Log().Debug("[OriginAndPivotMsg]Received a broadcast message")
	var data OriginAndPivotPack
	if err := msg.Decode(&data); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	// Deliver all to the downloader
	return backend.Handle(peer, &data)
}

func handlePPOSInfoMsg(backend Backend, msg Decoder, peer *Peer) error {
	peer.Log().Debug("Received a broadcast message[PPOSInfoMsg]")
	var data PposInfoPack
	if err := msg.Decode(&data); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	// Deliver all to the downloader
	return backend.Handle(peer, &data)
}