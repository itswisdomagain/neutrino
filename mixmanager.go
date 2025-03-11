package neutrino

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"decred.org/dcrwallet/v4/lru"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mixing"
	"github.com/btcsuite/btcd/mixing/mixpool"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/crypto/blake256"
)

const invLRUSize = 5000

// stallTimeout is the amount of time allowed before a request to receive data
// that is known to exist at the RemotePeer times out with no matching reply.
const stallTimeout = 30 * time.Second

var errUnrequestedMixMsg = fmt.Errorf("received unrequested mix msg")

type mixPeerState struct {
	requestedMixMsgsMu sync.Mutex
	requestedMixMsgs   map[chainhash.Hash]chan<- mixing.Message

	invsSent lru.Cache[chainhash.Hash] // Hashes from sent inventory messages
}

func newMixPeerState() *mixPeerState {
	return &mixPeerState{
		requestedMixMsgs: make(map[chainhash.Hash]chan<- mixing.Message),
		invsSent:         lru.NewCache[chainhash.Hash](invLRUSize),
	}
}

func (p *mixPeerState) addRequestedMixMsg(hash *chainhash.Hash, c chan<- mixing.Message) (newRequest bool) {
	p.requestedMixMsgsMu.Lock()
	_, ok := p.requestedMixMsgs[*hash]
	if !ok {
		p.requestedMixMsgs[*hash] = c
	}
	p.requestedMixMsgsMu.Unlock()
	return !ok
}

func (p *mixPeerState) deleteRequestedMixMsg(hash *chainhash.Hash) {
	p.requestedMixMsgsMu.Lock()
	delete(p.requestedMixMsgs, *hash)
	p.requestedMixMsgsMu.Unlock()
}

var (
	blake256Hasher = blake256.New()
	blake256Mu     sync.Mutex
)

func writeMixMsgHash(msg mixing.Message) chainhash.Hash {
	blake256Mu.Lock()
	defer blake256Mu.Unlock()

	msg.WriteHash(blake256Hasher)
	return msg.Hash()
}

func (ps *mixPeerState) receivedMixMsg(msg mixing.Message, quit <-chan struct{}) error {
	mixHash := writeMixMsgHash(msg)
	ps.requestedMixMsgsMu.Lock()
	c, ok := ps.requestedMixMsgs[mixHash]
	delete(ps.requestedMixMsgs, mixHash)
	ps.requestedMixMsgsMu.Unlock()
	if !ok {
		return errUnrequestedMixMsg
	}
	select {
	case c <- msg:
	case <-quit:
	}
	return nil
}

type mixManager struct {
	// mixWallet should be assigned when starting the blockManager to ensure
	// proper processing of mix messages received from peers. Mix messages will
	// be ignored if mixWallet is nil.
	mixWallet MixWallet

	// seenMixMsgs record hashes of received inventoried mix messages. Once a
	// message is fetched and processed from one peer, the hash is added to the
	// cache to avoid fetching it again from other peers that also announce it.
	seenMixMsgs lru.Cache[chainhash.Hash]

	peerStates *sync.Map // k=peerID, v=*mixPeerState
}

func startMixManager(mixWallet MixWallet) *mixManager {
	return &mixManager{
		mixWallet:   mixWallet,
		seenMixMsgs: lru.NewCache[chainhash.Hash](2000),
		peerStates:  &sync.Map{},
	}
}

// handleMixInvs processes mix inv hashes announced by a peer and requests the
// mix msgs from the peer if we don't already have them.
func (mm *mixManager) handleMixInvs(peer *peer.Peer, hashes []*chainhash.Hash,
	onlyByID map[[33]byte]struct{}, quit <-chan struct{}) {

	// TODO: Remove
	const opf = "spv.handleMixInvs(%v): %w"

	// Ignore already-processed messages
	unseen := hashes[:0]
	for _, h := range hashes {
		if !mm.seenMixMsgs.Contains(*h) {
			unseen = append(unseen, h)
		}
	}
	if len(unseen) == 0 {
		return
	}

	msgs, err := mm.requestMixMessagesFromPeer(peer, unseen, quit)
	if err != nil && err.Error() == "not found" { // TODO!: errors.Is(err, errors.NotExist)
		err = nil
		// Remove notfound txs.
		prevMsgs, prevUnseen := msgs, unseen
		msgs, unseen = msgs[:0], unseen[:0]
		for i, msg := range prevMsgs {
			if msg != nil {
				msgs = append(msgs, msg)
				unseen = append(unseen, prevUnseen[i])
			}
		}
	}
	if err != nil {
		err := fmt.Errorf(opf, peer.Addr(), err)
		log.Warn(err)
		return
	}

	// Mark messages as processed so they are not queried from other nodes
	// who announce them in the future.
	for _, h := range unseen {
		mm.seenMixMsgs.Add(*h)
	}

	requestUnknownPRs := make(map[chainhash.Hash]struct{})
	unknownPRIDs := make(map[[33]byte]struct{})

	// Accept mix messages to the wallet's mixpool.  If any KE was an
	// orphan and does not reference its own PR, request the previous
	// messages as well.
	for _, msg := range msgs {
		if len(onlyByID) != 0 {
			if _, ok := onlyByID[[33]byte(msg.Pub())]; !ok {
				continue
			}
		}

		err := mm.mixWallet.AcceptMixMessage(msg)
		var missingPRErr *mixpool.MissingOwnPRError
		if errors.As(err, &missingPRErr) {
			ke := msg.(*wire.MsgMixKeyExchange)
			log.Debugf("will request unknown PR from %x", ke.Identity[:])
			requestUnknownPRs[missingPRErr.MissingPR] = struct{}{}
			unknownPRIDs[ke.Identity] = struct{}{}
		} else if err != nil {
			log.Warn(fmt.Errorf(opf, peer.Addr(), err))
		}
	}

	if len(requestUnknownPRs) > 0 {
		requestUnknownPRs := make(map[chainhash.Hash]struct{})
		unknownPRs := make([]*chainhash.Hash, 0, len(requestUnknownPRs))
		for hash := range requestUnknownPRs {
			hash := hash
			unknownPRs = append(unknownPRs, &hash)
		}
		mm.handleMixInvs(peer, unknownPRs, unknownPRIDs, quit)
	}
}

// requestMixMessagesFromPeer requests multiple mixing messages at a time from a
// RemotePeer using a single getdata message.  It returns when all of the
// messages and/or notfound messages have been received.  The same message may
// not be requested multiple times concurrently from the same peer.  Returns
// ErrNotFound with a slice of one or more nil messages if any notfound messages
// are received for requested mix messages.
func (mm *mixManager) requestMixMessagesFromPeer(p *peer.Peer, hashes []*chainhash.Hash,
	quit <-chan struct{}) ([]mixing.Message, error) {

	opf := "remotepeer(%v).MixMessages: %w" // TODO

	psv, _ := mm.peerStates.LoadOrStore(p.ID(), newMixPeerState())
	ps := psv.(*mixPeerState)

	m := wire.NewMsgGetDataSizeHint(uint(len(hashes)))
	cs := make([]chan mixing.Message, len(hashes))
	for i, h := range hashes {
		err := m.AddInvVect(wire.NewInvVect(wire.InvTypeMix, h))
		if err != nil {
			return nil, fmt.Errorf(opf, p.Addr(), err)
		}
		cs[i] = make(chan mixing.Message, 1)
		if !ps.addRequestedMixMsg(h, cs[i]) {
			for _, h := range hashes[:i] {
				ps.deleteRequestedMixMsg(h)
			}
			err = fmt.Errorf("mix msg %v is already being requested from this peer", h)
			return nil, fmt.Errorf(opf, p.Addr(), err)
		}
	}

	p.QueueMessage(m, nil)

	msgs := make([]mixing.Message, len(hashes))
	var notfound bool
	stalled := time.NewTimer(stallTimeout)
	for i := 0; i < len(hashes); i++ {
		select {
		case <-quit:
			go func() {
				<-stalled.C
				for _, h := range hashes[i:] {
					ps.deleteRequestedMixMsg(h)
				}
			}()
			return nil, fmt.Errorf("mixmanager shut down")
		case <-stalled.C:
			for _, h := range hashes[i:] {
				ps.deleteRequestedMixMsg(h)
			}
			err := fmt.Errorf(opf, p.Addr(), fmt.Errorf("peer appears stalled"))
			p.Disconnect()
			return nil, err
		case <-p.DisconnectChan():
			stalled.Stop()
			return nil, fmt.Errorf("peer disconnected")
		case m, ok := <-cs[i]:
			msgs[i] = m
			notfound = notfound || !ok
		}
	}
	stalled.Stop()
	if notfound {
		return msgs, fmt.Errorf("not found")
	}
	return msgs, nil
}

func (mm *mixManager) receivedMixMessageFromPeer(p *peer.Peer, msg mixing.Message, quit <-chan struct{}) error {
	psv, ok := mm.peerStates.Load(p.ID())
	if !ok {
		return errUnrequestedMixMsg
	}

	ps := psv.(*mixPeerState)
	return ps.receivedMixMsg(msg, quit)
}

func (mm *mixManager) sendMixMsgInv(p *peer.Peer, msg *wire.MsgInv, ownPRs []*chainhash.Hash) {
	psv, _ := mm.peerStates.LoadOrStore(p.ID(), newMixPeerState())
	ps := psv.(*mixPeerState)

	for _, inv := range msg.InvList {
		ps.invsSent.Add(inv.Hash)
	}
	for _, prHash := range ownPRs {
		ps.invsSent.Add(*prHash)
	}

	p.QueueMessage(msg, nil)
}

func (mm *mixManager) handleMixMsgGetData(p *peer.Peer, hash *chainhash.Hash) (mixing.Message, error) {
	// Ensure that the data was (recently) announced using an inv.
	psv, ok := mm.peerStates.Load(p.ID())
	if !ok {
		return nil, errUnrequestedMixMsg
	}

	ps := psv.(*mixPeerState)
	if !ps.invsSent.Contains(*hash) {
		return nil, errUnrequestedMixMsg
	}

	return mm.mixWallet.MixMessage(hash)
}
