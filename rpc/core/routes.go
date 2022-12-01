package core

import (
	rpc "github.com/tendermint/tendermint/rpc/jsonrpc/server"
)

// TODO: better system than "unsafe" prefix

// Routes is a map of available routes.
var Routes = map[string]*rpc.RPCFunc{
	// subscribe/unsubscribe are reserved for websocket events.
	"subscribe":       rpc.NewWSRPCFunc(Subscribe, "query"),
	"unsubscribe":     rpc.NewWSRPCFunc(Unsubscribe, "query"),
	"unsubscribe_all": rpc.NewWSRPCFunc(UnsubscribeAll, ""),

	// info API
	"health":               rpc.NewRPCFunc(Health, ""),
	"status":               rpc.NewRPCFunc(Status, ""),
	"net_info":             rpc.NewRPCFunc(NetInfo, ""),
	"blockchain":           rpc.NewRPCFunc(BlockchainInfo, "chain_id,minHeight,maxHeight"),
	"genesis":              rpc.NewRPCFunc(Genesis, "chain_id"),
	"genesis_chunked":      rpc.NewRPCFunc(GenesisChunked, "chain_id,chunk"),
	"block":                rpc.NewRPCFunc(Block, "chain_id,height"),
	"block_by_hash":        rpc.NewRPCFunc(BlockByHash, "chain_id,hash"),
	"block_results":        rpc.NewRPCFunc(BlockResults, "chain_id,height"),
	"commit":               rpc.NewRPCFunc(Commit, "chain_id,height"),
	"check_tx":             rpc.NewRPCFunc(CheckTx, "tx"),
	"tx":                   rpc.NewRPCFunc(Tx, "hash,prove"),
	"tx_search":            rpc.NewRPCFunc(TxSearch, "chain_id,query,prove,page,per_page,order_by"),
	"block_search":         rpc.NewRPCFunc(BlockSearch, "chain_id,query,page,per_page,order_by"),
	"validators":           rpc.NewRPCFunc(Validators, "chain_id,height,page,per_page"),
	"dump_consensus_state": rpc.NewRPCFunc(DumpConsensusState, "chain_id"),
	"consensus_state":      rpc.NewRPCFunc(ConsensusState, "chain_id"),
	"consensus_params":     rpc.NewRPCFunc(ConsensusParams, "chain_id,height"),
	"unconfirmed_txs":      rpc.NewRPCFunc(UnconfirmedTxs, "limit"),
	"num_unconfirmed_txs":  rpc.NewRPCFunc(NumUnconfirmedTxs, ""),

	// tx broadcast API
	"broadcast_tx_commit": rpc.NewRPCFunc(BroadcastTxCommit, "tx"),
	"broadcast_tx_sync":   rpc.NewRPCFunc(BroadcastTxSync, "tx"),
	"broadcast_tx_async":  rpc.NewRPCFunc(BroadcastTxAsync, "tx"),

	// abci API
	"abci_query": rpc.NewRPCFunc(ABCIQuery, "chain_id,path,data,height,prove"),
	"abci_info":  rpc.NewRPCFunc(ABCIInfo, ""),

	// evidence API
	"broadcast_evidence": rpc.NewRPCFunc(BroadcastEvidence, "evidence"),
}

// AddUnsafeRoutes adds unsafe routes.
func AddUnsafeRoutes() {
	// control API
	Routes["dial_seeds"] = rpc.NewRPCFunc(UnsafeDialSeeds, "seeds")
	Routes["dial_peers"] = rpc.NewRPCFunc(UnsafeDialPeers, "peers,persistent,unconditional,private")
	Routes["unsafe_flush_mempool"] = rpc.NewRPCFunc(UnsafeFlushMempool, "")
}
