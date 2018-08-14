package peer

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/ioutil"
	"net"
	"sync"
	"time"

	"github.com/cenkalti/rain/internal/bitfield"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/internal/messageid"
)

const connReadTimeout = 3 * time.Minute

// Reject requests larger than this size.
const maxAllowedBlockSize = 32 * 1024

type Peer struct {
	conn      net.Conn
	id        [20]byte
	numPieces uint32

	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool

	// pieces that the peer has
	bitfield *bitfield.Bitfield

	messages     chan Message
	stopC        chan struct{}
	m            sync.Mutex
	disconnected chan struct{}
	log          logger.Logger
}

func New(conn net.Conn, id [20]byte, numPieces uint32, l logger.Logger, messages chan Message) *Peer {
	return &Peer{
		conn:         conn,
		id:           id,
		numPieces:    numPieces,
		amChoking:    true,
		peerChoking:  true,
		messages:     messages,
		stopC:        make(chan struct{}),
		disconnected: make(chan struct{}),
		log:          l,
	}
}

func (p *Peer) ID() [20]byte {
	return p.id
}

func (p *Peer) String() string {
	return p.conn.RemoteAddr().String()
}

func (p *Peer) NotifyDisconnect() chan struct{} {
	return p.disconnected
}

func (p *Peer) Close() error {
	close(p.stopC)
	return p.conn.Close()
}

// Run reads and processes incoming messages after handshake.
// TODO send keep-alive messages to peers at interval.
func (p *Peer) Run(b *bitfield.Bitfield) {
	p.log.Debugln("Communicating peer", p.conn.RemoteAddr())
	defer close(p.disconnected)

	if err := p.sendBitfield(b); err != nil {
		p.log.Error(err)
		return
	}

	first := true
	for {
		err := p.conn.SetReadDeadline(time.Now().Add(connReadTimeout))
		if err != nil {
			p.log.Error(err)
			return
		}

		var length uint32
		p.log.Debug("Reading message...")
		err = binary.Read(p.conn, binary.BigEndian, &length)
		if err != nil {
			if err == io.EOF {
				p.log.Debug("Remote peer has closed the connection")
			} else {
				p.log.Error(err)
			}
			return
		}
		p.log.Debugf("Received message of length: %d", length)

		if length == 0 { // keep-alive message
			p.log.Debug("Received message of type \"keep alive\"")
			continue
		}

		var id messageid.MessageID
		err = binary.Read(p.conn, binary.BigEndian, &id)
		if err != nil {
			p.log.Error(err)
			return
		}
		length--

		p.log.Debugf("Received message of type: %q", id)

		switch id {
		case messageid.Choke:
			p.m.Lock()
			p.peerChoking = true
			p.m.Unlock()
			select {
			case p.messages <- Message{p, Choke{}}:
			case <-p.stopC:
				return
			}
		case messageid.Unchoke:
			p.m.Lock()
			p.peerChoking = false
			p.m.Unlock()
			// TODO implement
		case messageid.Interested:
			p.m.Lock()
			p.peerInterested = true
			p.m.Unlock()
			// TODO implement
		case messageid.NotInterested:
			p.m.Lock()
			p.peerInterested = false
			p.m.Unlock()
			// TODO implement
		case messageid.Have:
			var h Have
			err = binary.Read(p.conn, binary.BigEndian, &h)
			if err != nil {
				p.log.Error(err)
				return
			}
			if h.Index >= p.numPieces {
				p.log.Error("unexpected piece index")
				return
			}
			p.log.Debug("Peer ", p.conn.RemoteAddr(), " has piece #", h.Index)
			p.bitfield.Set(h.Index)
			select {
			case p.messages <- Message{p, h}:
			case <-p.stopC:
				return
			}
		case messageid.Bitfield:
			if !first {
				p.log.Error("bitfield can only be sent after handshake")
				return
			}
			numBytes := uint32(bitfield.NumBytes(p.numPieces))
			if length != numBytes {
				p.log.Error("invalid bitfield length")
				return
			}
			b := make([]byte, numBytes)
			_, err = io.ReadFull(p.conn, b)
			if err != nil {
				p.log.Error(err)
				return
			}
			bf := bitfield.NewBytes(b, p.numPieces)
			p.m.Lock()
			p.bitfield = bf
			p.m.Unlock()
			p.log.Debugln("Received bitfield:", p.bitfield.Hex())

			for i := uint32(0); i < p.bitfield.Len(); i++ {
				if p.bitfield.Test(i) {
					select {
					case p.messages <- Message{p, Have{i}}:
					case <-p.stopC:
						return
					}
				}
			}
		// 	case messageid.Request:
		// 		var req requestMessage
		// 		err = binary.Read(p.conn, binary.BigEndian, &req)
		// 		if err != nil {
		// 			p.log.Error(err)
		// 			return
		// 		}
		// 		p.log.Debugf("Request: %+v", req)

		// 		if req.Index >= p.torrent.info.NumPieces {
		// 			p.log.Error("invalid request: index")
		// 			return
		// 		}
		// 		if req.Length > maxAllowedBlockSize {
		// 			p.log.Error("received a request with block size larger than allowed")
		// 			return
		// 		}
		// 		if req.Begin+req.Length > p.torrent.pieces[req.Index].Length {
		// 			p.log.Error("invalid request: length")
		// 		}

		// 		p.torrent.requestC <- &peerRequest{p, req}
		case messageid.Piece:
			var msg Piece
			err = binary.Read(p.conn, binary.BigEndian, &msg)
			if err != nil {
				p.log.Error(err)
				return
			}
			length -= 8

			// if msg.Index >= p.numPieces {
			// 	p.log.Error("invalid request: index")
			// 	return
			// }
			// piece := p.torrent.pieces[msg.Index]

			// // We request only in blockSize length
			// blockIndex, mod := divMod32(msg.Begin, blockSize)
			// if mod != 0 {
			// 	p.log.Error("unexpected block begin")
			// 	return
			// }
			// if blockIndex >= uint32(len(piece.Blocks)) {
			// 	p.log.Error("invalid block begin")
			// 	return
			// }
			// block := p.torrent.pieces[msg.Index].Blocks[blockIndex]
			// if length != block.Length {
			// 	p.log.Error("invalid piece block length")
			// 	return
			// }

			// p.torrent.m.Lock()
			// active := piece.GetRequest(p.id)
			// if active == nil {
			// 	p.torrent.m.Unlock()
			// 	p.log.Warning("received a piece that is not active")
			// 	continue
			// }

			// if active.BlocksReceiving.Test(block.Index) {
			// 	p.log.Warningf("Receiving duplicate block: Piece #%d Block #%d", piece.Index, block.Index)
			// } else {
			// 	active.BlocksReceiving.Set(block.Index)
			// }
			// p.torrent.m.Unlock()

			// if _, err = io.ReadFull(p.conn, active.Data[msg.Begin:msg.Begin+length]); err != nil {
			// 	p.log.Error(err)
			// 	return
			// }

			// p.torrent.m.Lock()
			// active.BlocksReceived.Set(block.Index)
			// if !active.BlocksReceived.All() {
			// 	p.torrent.m.Unlock()
			// 	p.cond.Broadcast()
			// 	continue
			// }
			// p.torrent.m.Unlock()

			// p.log.Debugf("Writing piece to disk: #%d", piece.Index)
			// if _, err = piece.Write(active.Data); err != nil {
			// 	p.log.Error(err)
			// 	// TODO remove errcheck ignore
			// 	p.conn.Close() // nolint: errcheck
			// 	return
			// }

			// p.torrent.m.Lock()
			// p.torrent.bitfield.Set(piece.Index)
			// percentDone := p.torrent.bitfield.Count() * 100 / p.torrent.bitfield.Len()
			// p.torrent.m.Unlock()
			// p.cond.Broadcast()
			// p.torrent.log.Infof("Completed: %d%%", percentDone)
		// // 	case messageid.Cancel:
		// // 	case messageid.Port:
		default:
			p.log.Debugf("unhandled message type: %s", id)
			p.log.Debugln("Discarding", length, "bytes...")
			_, err = io.CopyN(ioutil.Discard, p.conn, int64(length))
			if err != nil {
				p.log.Error(err)
				return
			}
		}
		first = false
	}
}

func (p *Peer) sendBitfield(b *bitfield.Bitfield) error {
	// Sending bitfield may be omitted if have no pieces.
	if b.Count() == 0 {
		return nil
	}
	return p.writeMessage(messageid.Bitfield, b.Bytes())
}

func (p *Peer) SendInterested() error {
	p.m.Lock()
	if p.amInterested {
		p.m.Unlock()
		return nil
	}
	p.amInterested = true
	p.m.Unlock()
	return p.writeMessage(messageid.Interested, nil)
}

func (p *Peer) SendNotInterested() error {
	p.m.Lock()
	if !p.amInterested {
		p.m.Unlock()
		return nil
	}
	p.amInterested = false
	p.m.Unlock()
	return p.writeMessage(messageid.NotInterested, nil)
}

func (p *Peer) SendChoke() error {
	p.m.Lock()
	if p.amChoking {
		p.m.Unlock()
		return nil
	}
	p.amChoking = true
	p.m.Unlock()
	return p.writeMessage(messageid.Choke, nil)
}

func (p *Peer) SendUnchoke() error {
	p.m.Lock()
	if !p.amChoking {
		p.m.Unlock()
		return nil
	}
	p.amChoking = false
	p.m.Unlock()
	return p.writeMessage(messageid.Unchoke, nil)
}

func (p *Peer) SendRequest(piece, begin, length uint32) error {
	req := Request{piece, begin, length}
	buf := bytes.NewBuffer(make([]byte, 0, 12))
	_ = binary.Write(buf, binary.BigEndian, &req)
	return p.writeMessage(messageid.Request, buf.Bytes())
}

func (p *Peer) SendPiece(index, begin uint32, block []byte) error {
	msg := Piece{index, begin}
	buf := bytes.NewBuffer(make([]byte, 0, 8))
	_ = binary.Write(buf, binary.BigEndian, msg)
	buf.Write(block)
	return p.writeMessage(messageid.Piece, buf.Bytes())
}

func (p *Peer) writeMessage(id messageid.MessageID, payload []byte) error {
	p.log.Debugf("Sending message of type: %q", id)
	buf := bytes.NewBuffer(make([]byte, 0, 4+1+len(payload)))
	var header = struct {
		Length uint32
		ID     messageid.MessageID
	}{
		uint32(1 + len(payload)),
		id,
	}
	_ = binary.Write(buf, binary.BigEndian, &header)
	buf.Write(payload)
	_, err := p.conn.Write(buf.Bytes())
	return err
}

func divMod32(a, b uint32) (uint32, uint32) { return a / b, a % b }
