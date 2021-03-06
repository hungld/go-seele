/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package p2p

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/p2p/discovery"
)

const (
	pingInterval         = 15 * time.Second // ping interval for peer tcp connection. Should be 15
	discAlreadyConnected = 10               // node already has connection
	discServerQuit       = 11               // p2p.server need quit, all peers should quit as it can
)

// Peer represents a connected remote node.
type Peer struct {
	protocolErr   chan error
	closed        chan struct{}
	Node          *discovery.Node // remote peer that this peer connects
	disconnection chan uint
	protocolMap   map[string]protocolRW // protocol cap => protocol read write wrapper
	rw            *connection

	wg  sync.WaitGroup
	log *log.SeeleLog
}

func NewPeer(conn *connection, protocols []Protocol, log *log.SeeleLog, node *discovery.Node) *Peer {
	closed := make(chan struct{})
	offset := baseProtoCode
	protoMap := make(map[string]protocolRW)
	for _, p := range protocols {
		protoRW := protocolRW{
			rw:       conn,
			offset:   offset,
			Protocol: p,
			in:       make(chan Message, 1),
			close:    closed,
		}

		protoMap[p.cap().String()] = protoRW
		offset += p.Length
		log.Debug("NewPeer called, add protocol: %s", p.cap())
	}

	return &Peer{
		rw:            conn,
		protocolMap:   protoMap,
		disconnection: make(chan uint),
		closed:        closed,
		log:           log,
		protocolErr:   make(chan error),
		Node:          node,
	}
}

func (p *Peer) getShardNumber() uint {
	return p.Node.Shard
}

// run assumes that SubProtocol will never quit, otherwise proto.DelPeerCh may be closed before peer.run quits?
func (p *Peer) run() (err error) {
	var readErr = make(chan error, 1)
	p.wg.Add(2)
	go p.readLoop(readErr)
	go p.pingLoop()

	p.notifyProtocols()
	// Wait for an error or disconnect.
errLoop:
	for {
		select {
		case err = <-readErr:
			p.log.Warn("p2p.peer.run read err %s", err.Error())
			break errLoop
		case <-p.disconnection:
			p.log.Info("p2p peer got disconnection request")
			err = errors.New("disconnection error received")
			break errLoop
		case err = <-p.protocolErr:
			p.log.Warn("p2p peer got protocol err %s", err.Error())
			break errLoop
		}
	}

	p.wg.Wait()
	p.close()
	p.log.Info("p2p.peer.run quit. err=%s", err)

	return err
}

func (p *Peer) close() {
	close(p.closed)
	close(p.disconnection)
}

func (p *Peer) pingLoop() {
	ping := time.NewTimer(pingInterval)
	defer p.wg.Done()
	defer ping.Stop()
	for {
		select {
		case <-ping.C:
			p.sendCtlMsg(ctlMsgPingCode)
			ping.Reset(pingInterval)
		case <-p.closed:
			return
		}
	}
}

func (p *Peer) readLoop(readErr chan<- error) {
	defer p.wg.Done()
	for {
		msgRecv, err := p.rw.ReadMsg()
		//p.log.Debug("got msg from peer: %s, code: %d",p.Node, msgRecv.Code)
		if err != nil {
			readErr <- err
			return
		}
		if err = p.handle(msgRecv); err != nil {
			readErr <- err
			return
		}
	}
}

func (p *Peer) notifyProtocols() {
	p.wg.Add(len(p.protocolMap))
	p.log.Debug("notifyProtocols called, len(protocolMap)=%d", len(p.protocolMap))
	for _, proto := range p.protocolMap {
		go func() {
			defer p.wg.Done()

			if proto.AddPeer != nil {
				p.log.Debug("notifyProtocols.AddPeer called. protocol:%s", proto.cap())
				proto.AddPeer(p, &proto)
			}
		}()
	}
}

func (p *Peer) handle(msgRecv Message) error {
	// control msg
	if msgRecv.Code < baseProtoCode {
		switch {
		case msgRecv.Code == ctlMsgPingCode:
			go p.sendCtlMsg(ctlMsgPongCode)
		case msgRecv.Code == ctlMsgPongCode:
			//p.log.Debug("peer handle Ping msg.")
			return nil
		case msgRecv.Code == ctlMsgDiscCode:
			return fmt.Errorf("error=%d", ctlMsgDiscCode)
		}

		return nil
	}

	var protocolTarget protocolRW
	found := false
	for _, p := range p.protocolMap {
		if msgRecv.Code >= p.offset && msgRecv.Code < p.offset+p.Length {
			protocolTarget = p
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf(fmt.Sprintf("could not found mapping proto with code %d", msgRecv.Code))
	}

	protocolTarget.in <- msgRecv

	return nil
}

func (p *Peer) sendCtlMsg(msgCode uint16) error {
	hsMsg := Message{
		Code: msgCode,
	}

	p.rw.WriteMsg(hsMsg)

	return nil
}

// Disconnect terminates the peer connection with the given reason.
// It returns immediately and does not wait until the connection is closed.
func (p *Peer) Disconnect(reason uint) {
	select {
	case p.disconnection <- reason:
	case <-p.closed:
	}
}

type protocolRW struct {
	Protocol
	offset uint16
	in     chan Message // read message channel, message will be transferred here when it is a protocol message
	rw     MsgReadWriter
	close  chan struct{}
}

func (rw *protocolRW) WriteMsg(msg Message) (err error) {
	if msg.Code >= rw.Length {
		return errors.New("invalid msg code")
	}

	msg.Code += rw.offset

	return rw.rw.WriteMsg(msg)
}

func (rw *protocolRW) ReadMsg() (Message, error) {
	select {
	case msg := <-rw.in:
		msg.Code -= rw.offset

		return msg, nil
	case <-rw.close:
		return Message{}, errors.New("peer connection closed")
	}
}

// ProtocolMap returns cap => protocol read write wrapper
func (p *Peer) ProtocolMap() map[string]protocolRW {
	return p.protocolMap
}

// RemoteAddr returns the remote address of the network connection.
func (p *Peer) RemoteAddr() net.Addr {
	return p.rw.fd.RemoteAddr()
}

// LocalAddr returns the local address of the network connection.
func (p *Peer) LocalAddr() net.Addr {
	return p.rw.fd.LocalAddr()
}

// PeerInfo represents a short summary of a connected peer
type PeerInfo struct {
	ID      string   `json:"id"`   // Unique of the node
	Caps    []string `json:"caps"` // Sum-protocols advertised by this particular peer
	Network struct {
		LocalAddress  string `json:"localAddress"`  // Local endpoint of the TCP data connection
		RemoteAddress string `json:"remoteAddress"` // Remote endpoint of the TCP data connection
	} `json:"network"`
	Protocols map[string]interface{} `json:"protocols"` // Sub-protocol specific metadata fields
}

// Info returns data of the peer but not contain id and name.
func (p *Peer) Info() *PeerInfo {
	var caps []string
	protocols := make(map[string]interface{})

	for cap, protocol := range p.ProtocolMap() {
		caps = append(caps, cap)

		protoInfo := interface{}("unknown")
		if query := protocol.Protocol.GetPeer; query != nil {
			if metadata := query(p.Node.ID); metadata != nil {
				protoInfo = metadata
			} else {
				protoInfo = "handshake"
			}
		}
		protocols[protocol.Protocol.Name] = protoInfo
	}

	info := &PeerInfo{
		ID:        p.Node.ID.ToHex(),
		Caps:      caps,
		Protocols: protocols,
	}
	info.Network.LocalAddress = p.LocalAddr().String()
	info.Network.RemoteAddress = p.RemoteAddr().String()

	return info
}
