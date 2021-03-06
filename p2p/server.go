/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package p2p

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/crypto"
	"github.com/seeleteam/go-seele/crypto/ecies"
	"github.com/seeleteam/go-seele/crypto/secp256k1"
	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/p2p/discovery"
)

const (
	// Maximun number of peers that can be connected
	defaultMaxPeers = 500

	// Maximum number of concurrently handshaking inbound connections.
	maxAcceptConns = 50

	defaultDialTimeout = 15 * time.Second

	// Maximum amount of time allowed for writing some bytes, not a complete message, because the message length is very highly variable.
	connWriteTimeout = 10 * time.Second

	// Maximum time allowed for reading a complete message.
	frameReadTimeout = 30 * time.Second

	// peerSyncDuration the duration of syncing peer info with node discovery, must bigger than discovery.discoveryInterval
	peerSyncDuration = 25 * time.Second

	inboundConn  = 1
	outboundConn = 2

	// In transfering handshake msg, length of extra data
	hsExtraDataLen = 32
)

//P2PConfig is the Configuration of p2p
type Config struct {
	// p2p.server will listen for incoming tcp connections. And it is for udp address used for Kad protocol
	ListenAddr string `json:"address"`

	// NetworkID used to define net type, for example main net and test net.
	NetworkID uint64 `json:"networkID"`

	// static nodes which will be connected to find more nodes when the node starts
	StaticNodes []*discovery.Node `json:"staticNodes"`

	// SubPrivateKey which will be make PrivateKey
	SubPrivateKey string `json:"privateKey"`

	// PrivateKey private key for p2p module, do not use it as any accounts
	PrivateKey *ecdsa.PrivateKey
}

// Server manages all p2p peer connections.
type Server struct {
	// Config fields may not be modified while the server is running.
	Config

	lock    sync.Mutex // protects running
	running bool

	kadDB    *discovery.Database
	listener net.Listener

	quit chan struct{}

	addPeerChan chan *Peer
	delPeerChan chan *Peer
	loopWG      sync.WaitGroup // loop, listenLoop

	peerMap      map[common.Address]*Peer
	shardPeerMap map[uint]map[common.Address]*Peer
	log          *log.SeeleLog

	// MaxPeers max number of peers that can be connected
	MaxPeers int

	// MaxPendingPeers is the maximum number of peers that can be pending in the
	// handshake phase, counted separately for inbound and outbound connections.
	// Zero defaults to preset values.
	MaxPendingPeers int

	// Protocols should contain the protocols supported by the server.
	Protocols []Protocol

	SelfNode *discovery.Node
}

func NewServer(config Config, protocols []Protocol) *Server {
	peers := make(map[uint]map[common.Address]*Peer)
	for i := 1; i < common.ShardNumber+1; i++ {
		peers[uint(i)] = make(map[common.Address]*Peer)
	}

	return &Server{
		Config:          config,
		running:         false,
		log:             log.GetLogger("p2p", common.LogConfig.PrintLog),
		MaxPeers:        defaultMaxPeers,
		peerMap:         make(map[common.Address]*Peer),
		quit:            make(chan struct{}),
		addPeerChan:     make(chan *Peer),
		delPeerChan:     make(chan *Peer),
		shardPeerMap:    peers,
		MaxPendingPeers: 0,
		Protocols:       protocols,
	}
}

// PeerCount return the count of peers
func (srv *Server) PeerCount() int {
	if srv.peerMap != nil {
		return len(srv.peerMap)
	}

	return 0
}

// Start starts running the server.
func (srv *Server) Start(shard uint) (err error) {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if srv.running {
		return errors.New("server already running")
	}

	srv.running = true
	srv.log.Info("Starting P2P networking...")
	// self node
	id := crypto.PubkeyToString(&srv.PrivateKey.PublicKey)
	address := common.HexMustToAddres(id)
	addr, err := net.ResolveUDPAddr("udp", srv.ListenAddr)

	srv.SelfNode = discovery.NewNodeWithAddr(address, addr, shard)
	if err != nil {
		return err
	}

	srv.log.Info("p2p.Server.Start: MyNodeID [%s]", srv.SelfNode)
	srv.kadDB = discovery.StartService(address, addr, srv.StaticNodes, shard)
	srv.kadDB.SetHookForNewNode(srv.addNode)

	if err := srv.startListening(); err != nil {
		return err
	}

	srv.loopWG.Add(1)
	go srv.run()
	srv.running = true
	return nil
}

func (srv *Server) addNode(node *discovery.Node) {
	if node.Shard == discovery.UndefinedShardNumber {
		return
	}

	srv.log.Info("got discovery a new node event, node info:%s", node)
	_, ok := srv.peerMap[node.ID]
	if ok {
		return
	}

	//TODO UDPPort==> TCPPort
	addr, _ := net.ResolveTCPAddr("tcp4", fmt.Sprintf("%s:%d", node.IP.String(), node.UDPPort))
	srv.log.Info("connecting to a new node... %s", addr.String())
	conn, err := net.DialTimeout("tcp", addr.String(), defaultDialTimeout)
	if err != nil {
		srv.log.Error("connect to a new node err: %s, node: %s", err, node)
		if conn != nil {
			conn.Close()
		}

		return
	}

	if err := srv.setupConn(conn, outboundConn, node); err != nil {
		srv.log.Info("add new node. setupConn called err returns. err=%s", err)
	}
}

func (srv *Server) addPeer(p *Peer) {
	srv.log.Info("server addPeer, len(peers)=%d", len(srv.peerMap))
	oldPeer, ok := srv.peerMap[p.Node.ID]

	if ok {
		srv.log.Info("peer already exists, disconnect it and update the new peer")
		p.Disconnect(discAlreadyConnected)

		peerMap := srv.shardPeerMap[oldPeer.getShardNumber()]
		delete(peerMap, oldPeer.Node.ID)
	}

	srv.shardPeerMap[p.getShardNumber()][p.Node.ID] = p
	srv.peerMap[p.Node.ID] = p

	metricsAddPeerMeter.Mark(1)
	metricsPeerCountGauge.Update(int64(len(srv.peerMap)))
}

func (srv *Server) deletePeer(p *Peer) {
	curPeer, ok := srv.peerMap[p.Node.ID]
	if ok && curPeer == p {
		delete(srv.peerMap, p.Node.ID)
		delete(srv.shardPeerMap[p.getShardNumber()], p.Node.ID)
		srv.log.Info("server.run delPeerChan recved. peer match. remove peer. peers num=%d", len(srv.peerMap))
		metricsDeletePeerMeter.Mark(1)
		metricsPeerCountGauge.Update(int64(len(srv.peerMap)))
	} else {
		srv.log.Info("server.run delPeerChan recved. peer not match")
	}
}

func (srv *Server) run() {
	defer srv.loopWG.Done()
	peerMap := srv.peerMap
	srv.log.Info("p2p start running...")

running:
	for {
		select {
		case <-srv.quit:
			srv.log.Warn("server got quit signal, run cleanup logic")
			break running
		case p := <-srv.addPeerChan:
			srv.addPeer(p)
		case p := <-srv.delPeerChan:
			srv.deletePeer(p)
		}
	}

	// Disconnect all peers.
	for _, p := range peerMap {
		p.Disconnect(discServerQuit)
	}

	for len(peerMap) > 0 {
		p := <-srv.delPeerChan
		srv.deletePeer(p)
	}
}

func (srv *Server) startListening() error {
	// Launch the TCP listener.
	listener, err := net.Listen("tcp", srv.Config.ListenAddr)
	if err != nil {
		return err
	}

	laddr := listener.Addr().(*net.TCPAddr)
	srv.Config.ListenAddr = laddr.String()
	srv.listener = listener
	srv.loopWG.Add(1)
	go srv.listenLoop()
	return nil
}

type tempError interface {
	Temporary() bool
}

// Wait wait for server until it exit
func (srv *Server) Wait() {
	srv.loopWG.Wait()
}

// listenLoop runs in its own goroutine and accepts inbound connections.
func (srv *Server) listenLoop() {
	defer srv.loopWG.Done()
	// If all slots are taken, no further connections are accepted.
	tokens := maxAcceptConns
	if srv.MaxPendingPeers > 0 {
		tokens = srv.MaxPendingPeers
	}
	slots := make(chan struct{}, tokens)
	for i := 0; i < tokens; i++ {
		slots <- struct{}{}
	}

	for {
		// Wait for a handshake slot before accepting.
		<-slots
		var (
			fd  net.Conn
			err error
		)
		for {
			fd, err = srv.listener.Accept()
			if tempErr, ok := err.(tempError); ok && tempErr.Temporary() {
				continue
			} else if err != nil {
				srv.log.Error("p2p.listenLoop accept err. %s", err)
				return
			}
			break
		}
		go func() {
			srv.log.Info("Accept new connection from, %s", fd.RemoteAddr())
			err := srv.setupConn(fd, inboundConn, nil)
			if err != nil {
				srv.log.Info("setupConn err, %s", err)
			}

			slots <- struct{}{}
		}()
	}
}

// setupConn Confirm both side are valid peers, have sub-protocols supported by each other
// Assume the inbound side is server side; outbound side is client side.
func (srv *Server) setupConn(fd net.Conn, flags int, dialDest *discovery.Node) error {
	srv.log.Info("setup connection with peer %s", dialDest)
	peer := NewPeer(&connection{fd: fd}, srv.Protocols, srv.log, dialDest)

	var caps []Cap
	for _, proto := range srv.Protocols {
		caps = append(caps, proto.cap())
	}

	recvMsg, nounceCnt, nounceSvr, err := srv.doHandShake(caps, peer, flags, dialDest)
	if err != nil {
		srv.log.Info("do handshake failed with peer %s, err info %s", dialDest, err)
		peer.close()
		return err
	}

	peerCaps, peerNodeID := recvMsg.Caps, recvMsg.NodeID
	if flags == inboundConn {
		peerNode, ok := srv.kadDB.FindByNodeID(peerNodeID)
		if !ok {
			srv.log.Info("p2p.setupConn conn handshaked, not found nodeID")
			peer.close()
			return errors.New("Not found nodeID in discovery database")
		}

		srv.log.Info("p2p.setupConn peerNodeID found in nodeMap. %s", peerNode.ID.ToHex())
		peer.Node = peerNode
	}

	srv.log.Debug("p2p.setupConn conn handshaked. nounceCnt=%d nounceSvr=%d peerCaps=%s", nounceCnt, nounceSvr, peerCaps)
	go func() {
		srv.loopWG.Add(1)
		srv.addPeerChan <- peer
		peer.run()
		srv.delPeerChan <- peer
		srv.loopWG.Done()
	}()

	return nil
}

// doHandShake Communicate each other
func (srv *Server) doHandShake(caps []Cap, peer *Peer, flags int, dialDest *discovery.Node) (recvMsg *ProtoHandShake, nounceCnt uint64, nounceSvr uint64, err error) {
	handshakeMsg := &ProtoHandShake{Caps: caps}
	nodeID := srv.SelfNode.ID
	copy(handshakeMsg.NodeID[0:], nodeID[0:])

	if flags == outboundConn {
		// client side. Send msg first
		binary.Read(rand.Reader, binary.BigEndian, &nounceCnt)
		wrapMsg, err := srv.packWrapHSMsg(handshakeMsg, dialDest.ID[0:], nounceCnt, nounceSvr)
		if err != nil {
			return nil, 0, 0, err
		}

		if err = peer.rw.WriteMsg(wrapMsg); err != nil {
			return nil, 0, 0, err
		}

		recvWrapMsg, err := peer.rw.ReadMsg()
		if err != nil {
			return nil, 0, 0, err
		}

		recvMsg, _, nounceSvr, err = srv.unPackWrapHSMsg(recvWrapMsg)
		if err != nil {
			return nil, 0, 0, err
		}
	} else {
		// server side. Recv handshake msg first
		binary.Read(rand.Reader, binary.BigEndian, &nounceSvr)
		recvWrapMsg, err := peer.rw.ReadMsg()
		if err != nil {
			return nil, 0, 0, err
		}

		recvMsg, nounceCnt, _, err = srv.unPackWrapHSMsg(recvWrapMsg)
		if err != nil {
			return nil, 0, 0, err
		}

		wrapMsg, err := srv.packWrapHSMsg(handshakeMsg, recvMsg.NodeID[0:], nounceCnt, nounceSvr)
		if err != nil {
			return nil, 0, 0, err
		}

		if err = peer.rw.WriteMsg(wrapMsg); err != nil {
			return nil, 0, 0, err
		}
	}
	return
}

// packWrapHSMsg compose the wrapped send msg.
// A 32 byte ExtraData is used for verification process.
func (srv *Server) packWrapHSMsg(handshakeMsg *ProtoHandShake, peerNodeID []byte, nounceCnt uint64, nounceSvr uint64) (Message, error) {
	// Serialize should handle big-endian
	hdmsgRLP, err := common.Serialize(handshakeMsg)
	if err != nil {
		return Message{}, err
	}
	wrapMsg := Message{
		Code: ctlMsgProtoHandshake,
	}
	md5Inst := md5.New()
	if _, err := md5Inst.Write(hdmsgRLP); err != nil {
		return Message{}, err
	}
	extBuf := make([]byte, hsExtraDataLen)
	// first 16 bytes, contains md5sum of hdmsgRLP;
	// then 8 bytes for client side nounce; 8 bytes for server side nounce
	copy(extBuf, md5Inst.Sum(nil))
	binary.BigEndian.PutUint64(extBuf[16:], nounceCnt)
	binary.BigEndian.PutUint64(extBuf[24:], nounceSvr)

	// 1. Sign with local privateKey first
	priKeyLocal := math.PaddedBigBytes(srv.PrivateKey.D, 32)
	sig, err := secp256k1.Sign(extBuf, priKeyLocal)
	if err != nil {
		return Message{}, err
	}
	// 2. Encrypt with peer publicKey
	pubObj := crypto.ToECDSAPub(peerNodeID[0:])
	remotePub := ecies.ImportECDSAPublic(pubObj)

	encOrg := make([]byte, hsExtraDataLen+len(sig))
	copy(encOrg, extBuf)
	copy(encOrg[hsExtraDataLen:], sig)
	enc, err := ecies.Encrypt(rand.Reader, remotePub, encOrg, nil, nil)
	if err != nil {
		return Message{}, err
	}

	// Format of wrapMsg payload, [handshake's rlp body, encoded extra data, length of encoded extra data]
	size := uint32(len(hdmsgRLP) + len(enc) + 4)
	wrapMsg.Payload = make([]byte, size)
	copy(wrapMsg.Payload, hdmsgRLP)
	copy(wrapMsg.Payload[len(hdmsgRLP):], enc)
	binary.BigEndian.PutUint32(wrapMsg.Payload[len(hdmsgRLP)+len(enc):], uint32(len(enc)))
	return wrapMsg, nil
}

// unPackWrapHSMsg verify recved msg, and recover the handshake msg
func (srv *Server) unPackWrapHSMsg(recvWrapMsg Message) (recvMsg *ProtoHandShake, nounceCnt uint64, nounceSvr uint64, err error) {
	size := uint32(len(recvWrapMsg.Payload))
	if size < hsExtraDataLen+4 {
		err = errors.New("recved err msg")
		return
	}
	extraEncLen := binary.BigEndian.Uint32(recvWrapMsg.Payload[size-4:])
	recvHSMsgLen := size - extraEncLen - 4
	nounceCnt = binary.BigEndian.Uint64(recvWrapMsg.Payload[recvHSMsgLen+16:])
	nounceSvr = binary.BigEndian.Uint64(recvWrapMsg.Payload[recvHSMsgLen+24:])
	recvEnc := recvWrapMsg.Payload[recvHSMsgLen : size-4]

	recvMsg = &ProtoHandShake{}
	if err = common.Deserialize(recvWrapMsg.Payload[:recvHSMsgLen], recvMsg); err != nil {
		return
	}

	// Decrypt with local private key, make sure it is sended to local
	eciesPriKey := ecies.ImportECDSA(srv.PrivateKey)
	encOrg, err := eciesPriKey.Decrypt(rand.Reader, recvEnc, nil, nil)
	if err != nil {
		return
	}

	// Verify peer public key, make sure it is sended from correct peer
	recvPubkey, err := secp256k1.RecoverPubkey(encOrg[0:hsExtraDataLen], encOrg[hsExtraDataLen:])
	if err != nil {
		return
	}

	if !bytes.Equal(recvMsg.NodeID[0:], recvPubkey[1:]) {
		err = errors.New("unPackWrapHSMsg: recvPubkey not match")
		return
	}

	// Verify recvMsg's payload md5sum to prevent modification
	md5Inst := md5.New()
	if _, err = md5Inst.Write(recvWrapMsg.Payload[:recvHSMsgLen]); err != nil {
		return
	}

	if !bytes.Equal(md5Inst.Sum(nil), encOrg[:16]) {
		err = errors.New("unPackWrapHSMsg: recved md5sum not match!")
		return
	}
	srv.log.Info("unPackWrapHSMsg: verify OK!")
	return
}

// Stop terminates the execution of the p2p server
func (srv *Server) Stop() {
	srv.lock.Lock()
	defer srv.lock.Unlock()

	if !srv.running {
		return
	}
	srv.running = false

	if srv.listener != nil {
		srv.listener.Close()
	}

	close(srv.quit)
	srv.Wait()
}

// PeerInfos array of PeerInfo for sort alphabetically by node identifier
type PeerInfos []PeerInfo

func (p PeerInfos) Len() int           { return len(p) }
func (p PeerInfos) Less(i, j int) bool { return p[i].ID < p[j].ID }
func (p PeerInfos) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// PeersInfo returns an array of metadata objects describing connected peers.
func (srv *Server) PeersInfo() *[]PeerInfo {
	infos := make([]PeerInfo, 0, srv.PeerCount())
	for _, peer := range srv.peerMap {
		if peer != nil {
			peerInfo := peer.Info()
			infos = append(infos, *peerInfo)
		}
	}
	sort.Sort(PeerInfos(infos))
	return &infos
}
