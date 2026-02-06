package rtc

import (
	"encoding/json"
	"sync"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/auth"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
)

const DefaultHops = 5

type PeerRole string

const (
	RoleClient PeerRole = "client"
	RoleServer PeerRole = "server"
)

type PeerListMessage struct {
	Type    string   `json:"type"`
	PeerIDs []string `json:"peer_ids"`
}

type RTCManager struct {
	sig      *signal.Client
	router   *signalingRouter
	selfID   string
	roomID   string
	selfRole PeerRole

	mu    sync.RWMutex
	peers map[string]*RemotePeer

	authorizer    auth.Authorizer
	authenticator auth.Authenticator

	chatHandlers        []func(string, string)
	tunnelMsgHandlers   []func(string, []byte)
	stdioMsgHandlers    []func(string, []byte)
	tunnelOpenHandlers  []func(string)
	stdioOpenHandlers   []func(string)
	tunnelCloseHandlers []func(string)
	stdioCloseHandlers  []func(string)
	peerConnHandlers    []func(string)
	authPathHandlers    []func(string, bool)
	quit                chan struct{}
}

func NewRTCManager(sig *signal.Client, selfID, roomID string, isServer bool) *RTCManager {
	role := RoleClient
	if isServer {
		role = RoleServer
	}
	m := &RTCManager{
		sig:      sig,
		selfID:   selfID,
		roomID:   roomID,
		selfRole: role,
		peers:    make(map[string]*RemotePeer),
		quit:     make(chan struct{}),
	}
	m.router = newSignalingRouter(selfID, m.relaySignal, m.processSignal)
	sig.OnMessage(m.router.receive)
	sig.OnReconnect(m.requestAllPeers)
	return m
}

func (m *RTCManager) SetAuthorizer(a auth.Authorizer)       { m.authorizer = a }
func (m *RTCManager) SetAuthenticator(a auth.Authenticator) { m.authenticator = a }

func (m *RTCManager) Close() {
	close(m.quit)
	m.router.stop()
	m.mu.Lock()
	peers := m.peers
	m.peers = make(map[string]*RemotePeer)
	m.mu.Unlock()
	for _, p := range peers {
		p.Close()
	}
	if m.sig != nil {
		m.sig.Close()
	}
}

func (m *RTCManager) getOrCreatePeer(peerID string) *RemotePeer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[peerID]; ok {
		return p
	}
	p := newRemotePeer(m, peerID)
	m.peers[peerID] = p
	return p
}

func (m *RTCManager) processSignal(msg signal.Message) {
	peer := m.getOrCreatePeer(msg.SenderId)
	if msg.Role != "" {
		peer.setRole(PeerRole(msg.Role))
	}

	m.mu.RLock()
	pc := peer.pc
	m.mu.RUnlock()

	signalingStable := pc != nil && pc.SignalingState() == webrtc.SignalingStateStable
	connActive := pc != nil && (pc.ConnectionState() == webrtc.PeerConnectionStateConnected ||
		pc.ConnectionState() == webrtc.PeerConnectionStateConnecting)

	switch msg.Type {
	case "Request":
		if signalingStable || connActive {
			return
		}
		if m.selfID < msg.SenderId {
			peer.startOffer()
		}

	case "offer":
		if signalingStable && connActive {
			return
		}

		if !signalingStable && pc != nil {
			if m.selfID < msg.SenderId {
				logger.Debug("[" + msg.SenderId + "] Glare: I am winner (Smaller ID), ignoring incoming offer")
				return
			}
			logger.Debug("[" + msg.SenderId + "] Glare: I am polite (Larger ID), yielding to their offer")
		}
		peer.handleOffer(msg.Data)

	case "answer":
		peer.handleAnswer(msg.Data)
	case "candidate":
		peer.handleCandidate(msg.Data)
	case "peer_list":
		var plMsg PeerListMessage
		if err := json.Unmarshal([]byte(msg.Data), &plMsg); err == nil {
			m.handlePeerList(plMsg.PeerIDs)
		}
	}
}

func (m *RTCManager) relaySignal(msg signal.Message) {
	msg.Hops--
	if msg.Hops < 0 {
		return
	}
	data, _ := json.Marshal(msg)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if msg.ReceiverId != "" {
		if p, ok := m.peers[msg.ReceiverId]; ok {
			if err := p.sendSignalRelay(data); err == nil {
				return
			}
		}
	}
	for id, p := range m.peers {
		if id != msg.SenderId && id != msg.ReceiverId {
			p.sendSignalRelay(data)
		}
	}
}

func (m *RTCManager) sendSignal(msg signal.Message) {
	if msg.MsgID == "" {
		msg.MsgID = nextMsgID(m.selfID)
	}
	if msg.Hops == 0 {
		msg.Hops = DefaultHops
	}
	m.relaySignal(msg)
	if msg.SenderId == m.selfID {
		m.sig.Send(msg)
	}
}

func (m *RTCManager) broadcastPeerList(targetPeerID string) {
	m.mu.RLock()
	ids := make([]string, 0)
	for id := range m.peers {
		if id != targetPeerID {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	if len(ids) == 0 {
		return
	}
	plData, _ := json.Marshal(PeerListMessage{Type: "peer_list", PeerIDs: ids})
	m.sendSignal(signal.Message{
		Type:       "peer_list",
		Data:       string(plData),
		SenderId:   m.selfID,
		ReceiverId: targetPeerID,
	})
}

func (m *RTCManager) handlePeerList(ids []string) {
	for _, id := range ids {
		if id == m.selfID {
			continue
		}
		m.mu.RLock()
		_, exists := m.peers[id]
		m.mu.RUnlock()
		if !exists {
			m.requestPeerConnection(id)
		}
	}
}

func (m *RTCManager) requestPeerConnection(peerID string) {
	m.sendSignal(signal.Message{Type: "Request", SenderId: m.selfID, ReceiverId: peerID, RoomId: m.roomID, Role: string(m.selfRole)})
}

func (m *RTCManager) requestAllPeers() {
	m.sig.Send(signal.Message{Type: "Request", SenderId: m.selfID, RoomId: m.roomID, Role: string(m.selfRole)})
}

func (m *RTCManager) handleRawRelay(data []byte) {
	var msg signal.Message
	if err := json.Unmarshal(data, &msg); err == nil {
		m.router.receive(msg)
	}
}

func (m *RTCManager) notifyChat(pID, msg string) {
	for _, h := range m.chatHandlers {
		h(pID, msg)
	}
}
func (m *RTCManager) notifyTunnelMsg(pID string, d []byte) {
	for _, h := range m.tunnelMsgHandlers {
		h(pID, d)
	}
}
func (m *RTCManager) notifyStdioMsg(pID string, d []byte) {
	for _, h := range m.stdioMsgHandlers {
		h(pID, d)
	}
}
func (m *RTCManager) notifyTunnelOpen(pID string) {
	for _, h := range m.tunnelOpenHandlers {
		h(pID)
	}
}
func (m *RTCManager) notifyStdioOpen(pID string) {
	for _, h := range m.stdioOpenHandlers {
		h(pID)
	}
}
func (m *RTCManager) notifyTunnelClose(pID string) {
	for _, h := range m.tunnelCloseHandlers {
		h(pID)
	}
}
func (m *RTCManager) notifyStdioClose(pID string) {
	for _, h := range m.stdioCloseHandlers {
		h(pID)
	}
}
func (m *RTCManager) notifyPeerConnected(pID string) {
	for _, h := range m.peerConnHandlers {
		h(pID)
	}
}
func (m *RTCManager) notifyAuth(pID string, success bool) {
	for _, h := range m.authPathHandlers {
		h(pID, success)
	}
}

func (m *RTCManager) OnChatMessage(h func(string, string)) {
	m.chatHandlers = append(m.chatHandlers, h)
}
func (m *RTCManager) OnTunnelMessage(h func(string, []byte)) {
	m.tunnelMsgHandlers = append(m.tunnelMsgHandlers, h)
}
func (m *RTCManager) OnStdioMessage(h func(string, []byte)) {
	m.stdioMsgHandlers = append(m.stdioMsgHandlers, h)
}
func (m *RTCManager) OnTunnelOpen(h func(string)) {
	m.tunnelOpenHandlers = append(m.tunnelOpenHandlers, h)
}
func (m *RTCManager) OnStdioOpen(h func(string)) {
	m.stdioOpenHandlers = append(m.stdioOpenHandlers, h)
}
func (m *RTCManager) OnTunnelClose(h func(string)) {
	m.tunnelCloseHandlers = append(m.tunnelCloseHandlers, h)
}
func (m *RTCManager) OnStdioClose(h func(string)) {
	m.stdioCloseHandlers = append(m.stdioCloseHandlers, h)
}
func (m *RTCManager) OnPeerConnected(h func(string)) {
	m.peerConnHandlers = append(m.peerConnHandlers, h)
}
func (m *RTCManager) OnAuth(h func(string, bool)) {
	m.authPathHandlers = append(m.authPathHandlers, h)
}

func (m *RTCManager) GetPeer(id string) *RemotePeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.peers[id]
}
func (m *RTCManager) GetAllPeers() []*RemotePeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ps := make([]*RemotePeer, 0, len(m.peers))
	for _, p := range m.peers {
		ps = append(ps, p)
	}
	return ps
}
func (m *RTCManager) GetConnectedPeerIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.peers))
	for id := range m.peers {
		ids = append(ids, id)
	}
	return ids
}
func (m *RTCManager) GetServerPeers() []*RemotePeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ps := make([]*RemotePeer, 0)
	for _, p := range m.peers {
		if p.IsServer() {
			ps = append(ps, p)
		}
	}
	return ps
}
func (m *RTCManager) SendChatToAll(msg string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.peers {
		p.SendChat(msg)
	}
}
func (m *RTCManager) SelfID() string { return m.selfID }
func (m *RTCManager) RoomID() string { return m.roomID }
