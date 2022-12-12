package v0

import (
	"fmt"
	"reflect"
	"time"
	"errors"

	bc "github.com/tendermint/tendermint/blockchain"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	bcproto "github.com/tendermint/tendermint/proto/tendermint/blockchain"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/store"
	"github.com/tendermint/tendermint/types"
)

const (
	// BlockchainChannel is a channel for blocks and status updates (`BlockStore` height)
	BlockchainChannel = byte(0x40)

	trySyncIntervalMS = 10

	// stop syncing when last block's time is
	// within this much of the system time.
	// stopSyncingDurationMinutes = 10

	// ask for best height every 10s
	statusUpdateIntervalSeconds = 10
	// check if we should switch to consensus reactor
	switchToConsensusIntervalSeconds = 1
)

type consensusReactor interface {
	// for when we switch from blockchain reactor and fast sync to
	// the consensus machine
	SwitchToConsensus(state sm.State, skipWAL bool)
}

type peerError struct {
	err    error
	peerID p2p.ID
}

func (e peerError) Error() string {
	return fmt.Sprintf("error with peer %v: %s", e.peerID, e.err.Error())
}

// BlockchainReactor handles long-term catchup syncing.
type BlockchainReactor struct {
	p2p.BaseReactor

	// immutable
	initialStateMap map[string]sm.State

	blockExecMap map[string]*sm.BlockExecutor
	storeMap     map[string]*store.BlockStore
	poolMap      map[string]*BlockPool
	fastSyncMap  map[string]bool

	requestsCh <-chan BlockRequest
	errorsCh   <-chan peerError
}

// NewBlockchainReactor returns new reactor instance.
func NewBlockchainReactor(stateMap map[string]sm.State, blockExecMap map[string]*sm.BlockExecutor, storeMap map[string]*store.BlockStore,
	fastSyncMap map[string]bool) *BlockchainReactor {

	requestsCh := make(chan BlockRequest, maxTotalRequesters)
	const capacity = 1000                      // must be bigger than peers count
	errorsCh := make(chan peerError, capacity) // so we don't block in #Receive#pool.AddBlock

	poolMap := make(map[string]*BlockPool)
	for chainID, state := range stateMap {
		store, found := storeMap[chainID]
		if !found {
			panic(fmt.Sprintf("store for chainID %s not found", chainID))
		}
		if state.LastBlockHeight != store.Height() {
			panic(fmt.Sprintf("state (%v) and store (%v) height mismatch", state.LastBlockHeight,
				store.Height()))
		}

		startHeight := store.Height() + 1
		if startHeight == 1 {
			startHeight = state.InitialHeight
		}
		pool := NewBlockPool(startHeight, requestsCh, errorsCh, chainID)
		poolMap[chainID] = pool
	}

	bcR := &BlockchainReactor{
		initialStateMap: stateMap,
		blockExecMap:    blockExecMap,
		storeMap:        storeMap,
		poolMap:         poolMap,
		fastSyncMap:     fastSyncMap,
		requestsCh:      requestsCh,
		errorsCh:        errorsCh,
	}
	bcR.BaseReactor = *p2p.NewBaseReactor("BlockchainReactor", bcR)
	return bcR
}

// SetLogger implements service.Service by setting the logger on reactor and pool.
func (bcR *BlockchainReactor) SetLogger(l log.Logger) {
	bcR.BaseService.Logger = l
	for _, pool := range bcR.poolMap {
		pool.Logger = l
	}
}

// OnStart implements service.Service.
func (bcR *BlockchainReactor) OnStart() error {
	for chainID, fastSync := range bcR.fastSyncMap {
		if !fastSync {
			continue
		}

		pool, found := bcR.poolMap[chainID];
		if !found {
			return errors.New(fmt.Sprintf("pool for chainID %s not found", chainID))
		}

		err := pool.Start()
		if err != nil {
			return err
		}
	}

	go bcR.poolRoutine(false)
	return nil
}

// SwitchToFastSync is called by the state sync reactor when switching to fast sync.
func (bcR *BlockchainReactor) SwitchToFastSync(state sm.State, chainID string) error {
	bcR.fastSyncMap[chainID] = true
	bcR.initialStateMap[chainID] = state

	pool, found := bcR.poolMap[chainID]; if !found {
		return fmt.Errorf("failed to retrieve pool for chainID %s", chainID)
	}
	pool.height = state.LastBlockHeight + 1
	err := pool.Start()
	if err != nil {
		return err
	}
	go bcR.poolRoutine(true)
	return nil
}

// OnStop implements service.Service.
func (bcR *BlockchainReactor) OnStop() {
	for chainID, fastSync := range bcR.fastSyncMap {
		if !fastSync {
			continue
		}

		pool, found := bcR.poolMap[chainID]; if !found {
			//TODO: write something into log
			continue
		}

		if err := pool.Stop(); err != nil {
			bcR.Logger.Error("Error stopping pool", "err", err)
		}
	}
}

// GetChannels implements Reactor
func (bcR *BlockchainReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		{
			ID:                  BlockchainChannel,
			Priority:            5,
			SendQueueCapacity:   1000,
			RecvBufferCapacity:  50 * 4096,
			RecvMessageCapacity: bc.MaxMsgSize,
		},
	}
}

// AddPeer implements Reactor by sending our state to peer.
func (bcR *BlockchainReactor) AddPeer(peer p2p.Peer) {
	for chainID, store := range bcR.storeMap {     // should be "for chainID, store :=...."
		msgBytes, err := bc.EncodeMsg(&bcproto.StatusResponse{
			Base:   store.Base(),
			Height: store.Height(),
			ChainID: chainID})             // Added by Yi
		if err != nil {
			bcR.Logger.Error("could not convert msg to protobuf", "err", err)
			return
		}

		peer.Send(BlockchainChannel, msgBytes)
		// it's OK if send fails. will try later in poolRoutine

		// peer is added to the pool once we receive the first
		// bcStatusResponseMessage from the peer and call pool.SetPeerRange
	}
}

// RemovePeer implements Reactor by removing peer from the pool.
func (bcR *BlockchainReactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	for _, pool:= range bcR.poolMap {
		pool.RemovePeer(peer.ID())   //YITODO: may need to reconsider whethere each pool associates with a pool
	}
}

// respondToPeer loads a block and sends it to the requesting peer,
// if we have it. Otherwise, we'll respond saying we don't have it.
func (bcR *BlockchainReactor) respondToPeer(msg *bcproto.BlockRequest,
	src p2p.Peer) (queued bool) {
	chainID := msg.ChainID
	store, found := bcR.storeMap[chainID]; if !found {
		bcR.Logger.Error("could not retrieve store for chainID ", chainID)
		return false
	}

	block := store.LoadBlock(msg.Height)
	if block != nil {
		bl, err := block.ToProto()
		if err != nil {
			bcR.Logger.Error("could not convert msg to protobuf", "err", err)
			return false
		}

		msgBytes, err := bc.EncodeMsg(&bcproto.BlockResponse{Block: bl, ChainID: chainID})
		if err != nil {
			bcR.Logger.Error("could not marshal msg", "err", err)
			return false
		}

		return src.TrySend(BlockchainChannel, msgBytes)
	}

	bcR.Logger.Info("Peer asking for a block we don't have", "src", src, "height", msg.Height)

	msgBytes, err := bc.EncodeMsg(&bcproto.NoBlockResponse{Height: msg.Height, ChainID: chainID})
	if err != nil {
		bcR.Logger.Error("could not convert msg to protobuf", "err", err)
		return false
	}

	return src.TrySend(BlockchainChannel, msgBytes)
}

// Receive implements Reactor by handling 4 types of messages (look below).
func (bcR *BlockchainReactor) Receive(chID byte, src p2p.Peer, msgBytes []byte) {
	msg, err := bc.DecodeMsg(msgBytes)
	if err != nil {
		bcR.Logger.Error("Error decoding message", "src", src, "chId", chID, "err", err)
		bcR.Switch.StopPeerForError(src, err)
		return
	}

	if err = bc.ValidateMsg(msg); err != nil {
		bcR.Logger.Error("Peer sent us invalid msg", "peer", src, "msg", msg, "err", err)
		bcR.Switch.StopPeerForError(src, err)
		return
	}

	bcR.Logger.Debug("Receive", "src", src, "chID", chID, "msg", msg)

	switch msg := msg.(type) {
	case *bcproto.BlockRequest:
		bcR.respondToPeer(msg, src)
	case *bcproto.BlockResponse:
		bi, err := types.BlockFromProto(msg.Block)
		if err != nil {
			bcR.Logger.Error("Block content is invalid", "err", err)
			return
		}
		chainID := msg.ChainID
		pool, found := bcR.poolMap[chainID]; if !found {
			return
		}
		pool.AddBlock(src.ID(), bi, len(msgBytes))
	case *bcproto.StatusRequest:
		chainID := msg.ChainID
		store, found := bcR.storeMap[chainID]; if !found {
			return
		}

		// Send peer our state.
		msgBytes, err := bc.EncodeMsg(&bcproto.StatusResponse{
			Height:  store.Height(),
			Base:    store.Base(),
			ChainID: chainID,
		})
		if err != nil {
			bcR.Logger.Error("could not convert msg to protobut", "err", err)
			return
		}
		src.TrySend(BlockchainChannel, msgBytes)
	case *bcproto.StatusResponse:
		chainID := msg.ChainID
		pool, found := bcR.poolMap[chainID]; if !found {
			return
		}
		// Got a peer status. Unverified.
		pool.SetPeerRange(src.ID(), msg.Base, msg.Height)
	case *bcproto.NoBlockResponse:
		bcR.Logger.Debug("Peer does not have requested block", "peer", src, "height", msg.Height)
	default:
		bcR.Logger.Error(fmt.Sprintf("Unknown message type %v", reflect.TypeOf(msg)))
	}
}

// Handle messages from the poolReactor telling the reactor what to do.
// NOTE: Don't sleep in the FOR_LOOP or otherwise slow it down!
func (bcR *BlockchainReactor) poolRoutine(stateSynced bool) {
	statusUpdateTicker := time.NewTicker(statusUpdateIntervalSeconds * time.Second)
	defer statusUpdateTicker.Stop()

	go func() {
		for {
			select {
			case <-bcR.Quit():
				return

			//YITODO: the following 2 lines of code might be needed in the future
			//case <-bcR.pool.Quit():
			//	return

			case request := <-bcR.requestsCh:
				peer := bcR.Switch.Peers().Get(request.PeerID)
				if peer == nil {
					continue
				}
				msgBytes, err := bc.EncodeMsg(&bcproto.BlockRequest{Height: request.Height, ChainID: request.ChainID})
				if err != nil {
					bcR.Logger.Error("could not convert msg to proto", "err", err)
					continue
				}

				queued := peer.TrySend(BlockchainChannel, msgBytes)
				if !queued {
					bcR.Logger.Debug("Send queue is full, drop block request", "peer", peer.ID(), "height", request.Height)
				}
			case err := <-bcR.errorsCh:
				peer := bcR.Switch.Peers().Get(err.peerID)
				if peer != nil {
					bcR.Switch.StopPeerForError(peer, err)
				}

			case <-statusUpdateTicker.C:
				// ask for status updates
				go bcR.BroadcastStatusRequest() // nolint: errcheck

			}
		}
	}()

	for chainID, state := range bcR.initialStateMap {
		go bcR.poolRoutineRaw(chainID, state, stateSynced)
	}
}

func (bcR *BlockchainReactor) poolRoutineRaw(chainID string, state sm.State, stateSynced bool) {
	trySyncTicker := time.NewTicker(trySyncIntervalMS * time.Millisecond)
	defer trySyncTicker.Stop()

	switchToConsensusTicker := time.NewTicker(switchToConsensusIntervalSeconds * time.Second)
	defer switchToConsensusTicker.Stop()

	blocksSynced := uint64(0)

	lastHundred := time.Now()
	lastRate := 0.0

	didProcessCh := make(chan struct{}, 1)

	pool, found := bcR.poolMap[chainID]; if !found {
		return     //YITODO: log before returning
	}
	store, found := bcR.storeMap[chainID]; if !found {
		return     //YITODO: log before returning
	}
	blockExec, found := bcR.blockExecMap[chainID]; if !found {
		return     //YITODO: log before returning
	}

FOR_LOOP:
	for {
		select {
		case <-switchToConsensusTicker.C:
			height, numPending, lenRequesters := pool.GetStatus()
			outbound, inbound, _ := bcR.Switch.NumPeers()
			bcR.Logger.Debug("Consensus ticker", "numPending", numPending, "total", lenRequesters,
				"outbound", outbound, "inbound", inbound)
			if pool.IsCaughtUp() {
				bcR.Logger.Info("Time to switch to consensus reactor!", "height", height)
				if err := pool.Stop(); err != nil {
					bcR.Logger.Error("Error stopping pool", "err", err)
				}
				conR, ok := bcR.Switch.Reactor("CONSENSUS").(consensusReactor)
				if ok {
					conR.SwitchToConsensus(state, blocksSynced > 0 || stateSynced)
				}
				// else {
				// should only happen during testing
				// }

				break FOR_LOOP
			}

		case <-trySyncTicker.C: // chan time
			select {
			case didProcessCh <- struct{}{}:
			default:
			}

		case <-didProcessCh:
			// NOTE: It is a subtle mistake to process more than a single block
			// at a time (e.g. 10) here, because we only TrySend 1 request per
			// loop.  The ratio mismatch can result in starving of blocks, a
			// sudden burst of requests and responses, and repeat.
			// Consequently, it is better to split these routines rather than
			// coupling them as it's written here.  TODO uncouple from request
			// routine.

			// See if there are any blocks to sync.
			first, second := pool.PeekTwoBlocks()
			// bcR.Logger.Info("TrySync peeked", "first", first, "second", second)
			if first == nil || second == nil {
				// We need both to sync the first block.
				continue FOR_LOOP
			} else {
				// Try again quickly next loop.
				didProcessCh <- struct{}{}
			}

			firstParts := first.MakePartSet(types.BlockPartSizeBytes)
			firstPartSetHeader := firstParts.Header()
			firstID := types.BlockID{Hash: first.Hash(), PartSetHeader: firstPartSetHeader}
			// Finally, verify the first block using the second's commit
			// NOTE: we can probably make this more efficient, but note that calling
			// first.Hash() doesn't verify the tx contents, so MakePartSet() is
			// currently necessary.
			err := state.Validators.VerifyCommitLight(
				chainID, firstID, first.Height, second.LastCommit)
			if err != nil {
				bcR.Logger.Error("Error in validation", "err", err)
				peerID := pool.RedoRequest(first.Height)
				peer := bcR.Switch.Peers().Get(peerID)
				if peer != nil {
					// NOTE: we've already removed the peer's request, but we
					// still need to clean up the rest.
					bcR.Switch.StopPeerForError(peer, fmt.Errorf("blockchainReactor validation error: %v", err))
				}
				peerID2 := pool.RedoRequest(second.Height)
				peer2 := bcR.Switch.Peers().Get(peerID2)
				if peer2 != nil && peer2 != peer {
					// NOTE: we've already removed the peer's request, but we
					// still need to clean up the rest.
					bcR.Switch.StopPeerForError(peer2, fmt.Errorf("blockchainReactor validation error: %v", err))
				}
				continue FOR_LOOP
			} else {
				pool.PopRequest()

				// TODO: batch saves so we dont persist to disk every block
				store.SaveBlock(first, firstParts, second.LastCommit)

				// TODO: same thing for app - but we would need a way to
				// get the hash without persisting the state
				var err error
				state, _, err = blockExec.ApplyBlock(state, firstID, first)
				if err != nil {
					// TODO This is bad, are we zombie?
					panic(fmt.Sprintf("Failed to process committed block (%d:%X): %v", first.Height, first.Hash(), err))
				}
				blocksSynced++

				if blocksSynced%100 == 0 {
					lastRate = 0.9*lastRate + 0.1*(100/time.Since(lastHundred).Seconds())
					bcR.Logger.Info("Fast Sync Rate", "height", pool.height,
						"max_peer_height", pool.MaxPeerHeight(), "blocks/s", lastRate)
					lastHundred = time.Now()
				}
			}
			continue FOR_LOOP

		case <-bcR.Quit():
			break FOR_LOOP
		}
	}
}

// BroadcastStatusRequest broadcasts `BlockStore` base and height.
func (bcR *BlockchainReactor) BroadcastStatusRequest() error {
	for chainID, _ := range bcR.storeMap {
		bm, err := bc.EncodeMsg(&bcproto.StatusRequest{ChainID: chainID})
		if err != nil {
			bcR.Logger.Error("could not convert msg to proto", "err", err)
			return fmt.Errorf("could not convert msg to proto: %w", err)
		}

		bcR.Switch.Broadcast(BlockchainChannel, bm)
	}
	return nil
}
