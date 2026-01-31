package rtc

import (
	"encoding/json"
	"sync"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
)

type PeerRole string

const (
	RoleClient PeerRole = "client"
	RoleServer PeerRole = "server"
)

type SignalMessage struct {
	Type       string   `json:"type"`
	Data       string   `json:"data,omitempty"`
	SenderId   string   `json:"sender_id"`
	ReceiverId string   `json:"receiver_id,omitempty"`
	RoomId     string   `json:"room_id,omitempty"`
	Role       PeerRole `json:"role,omitempty"`
}

type PeerListMessage struct {
	Type    string   `json:"type"`
	PeerIDs []string `json:"peer_ids"`
}

type RTCManager struct {
	sig      *signal.Client
	selfID   string
	roomID   string
	selfRole PeerRole

	mu    sync.RWMutex
	peers map[string]*RemotePeer

	globalChatCallbacks          []func(peerID string, msg string)
	globalTunnelMessageCallbacks []func(peerID string, msg []byte)
	globalTunnelOpenCallbacks    []func(peerID string)
	globalTunnelCloseCallbacks   []func(peerID string)
	globalPeerConnectedCallbacks []func(peerID string)
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
	}

	sig.OnMessage(m.handleWebSocketSignal)

	sig.OnReconnect(func() {
		logger.Debug("WebSocket reconnected, re-requesting peer connections")
		m.requestAllPeers()
	})

	logger.Debug("created RTCManager: " + selfID + " (role: " + string(role) + ")")
	return m
}

func (m *RTCManager) IsServer() bool {
	return m.selfRole == RoleServer
}

func (m *RTCManager) SelfID() string {
	return m.selfID
}

func (m *RTCManager) RoomID() string {
	return m.roomID
}

func (m *RTCManager) GetPeer(peerID string) *RemotePeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.peers[peerID]
}

func (m *RTCManager) GetAllPeers() []*RemotePeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*RemotePeer, 0, len(m.peers))
	for _, p := range m.peers {
		result = append(result, p)
	}
	return result
}

func (m *RTCManager) GetConnectedPeerIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, 0, len(m.peers))
	for id := range m.peers {
		result = append(result, id)
	}
	return result
}

func (m *RTCManager) GetServerPeers() []*RemotePeer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*RemotePeer, 0)
	for _, p := range m.peers {
		if p.IsServer() {
			result = append(result, p)
		}
	}
	return result
}

func (m *RTCManager) getOrCreatePeer(peerID string) *RemotePeer {
	m.mu.Lock()
	defer m.mu.Unlock()

	if peer, ok := m.peers[peerID]; ok {
		return peer
	}

	peer := newRemotePeer(m, peerID)
	m.peers[peerID] = peer

	peer.OnChatMessage(func(msg string) {
		for _, cb := range m.globalChatCallbacks {
			cb(peerID, msg)
		}
	})
	peer.OnTunnelOpen(func() {
		for _, cb := range m.globalTunnelOpenCallbacks {
			cb(peerID)
		}
	})
	peer.OnTunnelClose(func() {
		for _, cb := range m.globalTunnelCloseCallbacks {
			cb(peerID)
		}
	})
	peer.OnTunnelMessage(func(msg webrtc.DataChannelMessage) {
		for _, cb := range m.globalTunnelMessageCallbacks {
			cb(peerID, msg.Data)
		}
	})

	logger.Debug("Created new RemotePeer: " + peerID)
	return peer
}

func (m *RTCManager) handleWebSocketSignal(msg signal.Message) {
	logger.Debug("Received WebSocket signal: " + msg.Type + " from " + msg.SenderId)

	if msg.ReceiverId != "" && msg.ReceiverId != m.selfID {
		m.relaySignal(SignalMessage{
			Type:       msg.Type,
			Data:       msg.Data,
			SenderId:   msg.SenderId,
			ReceiverId: msg.ReceiverId,
			RoomId:     msg.RoomId,
			Role:       PeerRole(msg.Role),
		})
		return
	}

	m.processSignal(SignalMessage{
		Type:       msg.Type,
		Data:       msg.Data,
		SenderId:   msg.SenderId,
		ReceiverId: msg.ReceiverId,
		RoomId:     msg.RoomId,
		Role:       PeerRole(msg.Role),
	})
}

func (m *RTCManager) handleRelayedSignal(data []byte) {
	var msg SignalMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		logger.Debug("Failed to unmarshal relayed signal: " + err.Error())
		return
	}

	logger.Debug("Received relayed signal: " + msg.Type + " from " + msg.SenderId)

	if msg.ReceiverId != "" && msg.ReceiverId != m.selfID {
		m.relaySignal(msg)
		return
	}

	m.processSignal(msg)
}

func (m *RTCManager) processSignal(msg SignalMessage) {
	switch msg.Type {
	case "Request":
		// existingPeer check removed to allow re-connection/discovery attempts
		peer := m.getOrCreatePeer(msg.SenderId)
		if msg.Role != "" {
			peer.setRole(msg.Role)
			logger.Debug("Peer " + msg.SenderId + " role: " + string(msg.Role))
		}
		if m.selfID < msg.SenderId {
			if err := peer.startOffer(); err != nil {
				logger.Debug("Failed to start offer: " + err.Error())
			}
		}

	case "offer":
		peer := m.getOrCreatePeer(msg.SenderId)
		if msg.Role != "" {
			peer.setRole(msg.Role)
		}
		if err := peer.handleOffer(msg.Data); err != nil {
			logger.Debug("Failed to handle offer: " + err.Error())
		}

	case "answer":
		peer := m.GetPeer(msg.SenderId)
		if peer != nil {
			if msg.Role != "" {
				peer.setRole(msg.Role)
			}
			if err := peer.handleAnswer(msg.Data); err != nil {
				logger.Debug("Failed to handle answer: " + err.Error())
			}
		}

	case "candidate":
		peer := m.GetPeer(msg.SenderId)
		if peer != nil {
			if err := peer.handleCandidate(msg.Data); err != nil {
				logger.Debug("Failed to handle candidate: " + err.Error())
			}
		}

	case "peer_list":
		var plMsg PeerListMessage
		if err := json.Unmarshal([]byte(msg.Data), &plMsg); err != nil {
			logger.Debug("Failed to unmarshal peer_list: " + err.Error())
			return
		}
		m.handlePeerList(plMsg.PeerIDs)
	}
}

func (m *RTCManager) relaySignal(msg SignalMessage) {
	m.mu.RLock()
	targetPeer := m.peers[msg.ReceiverId]
	m.mu.RUnlock()

	if targetPeer != nil {
		data, _ := json.Marshal(msg)
		if err := targetPeer.sendSignalRelay(data); err == nil {
			logger.Debug("Relayed signal to " + msg.ReceiverId + " via DataChannel")
			return
		}
	}

	m.mu.RLock()
	for _, peer := range m.peers {
		if peer.peerID != msg.SenderId && peer.peerID != msg.ReceiverId {
			data, _ := json.Marshal(msg)
			if err := peer.sendSignalRelay(data); err == nil {
				logger.Debug("Forwarded signal to " + msg.ReceiverId + " via " + peer.peerID)
				break
			}
		}
	}
	m.mu.RUnlock()
}

func (m *RTCManager) sendSignal(msg SignalMessage) {
	m.mu.RLock()
	targetPeer := m.peers[msg.ReceiverId]
	m.mu.RUnlock()

	if targetPeer != nil {
		data, _ := json.Marshal(msg)
		if err := targetPeer.sendSignalRelay(data); err == nil {
			logger.Debug("Sent signal to " + msg.ReceiverId + " via DataChannel")
			return
		}
	}

	m.sig.Send(signal.Message{
		Type:       msg.Type,
		Data:       msg.Data,
		SenderId:   msg.SenderId,
		ReceiverId: msg.ReceiverId,
		RoomId:     msg.RoomId,
		Role:       string(msg.Role),
	})
}

func (m *RTCManager) broadcastPeerList(targetPeerID string) {
	m.mu.RLock()
	peerIDs := make([]string, 0, len(m.peers))
	for id := range m.peers {
		if id != targetPeerID {
			peerIDs = append(peerIDs, id)
		}
	}
	targetPeer := m.peers[targetPeerID]
	m.mu.RUnlock()

	if len(peerIDs) == 0 || targetPeer == nil {
		return
	}

	plMsg := PeerListMessage{
		Type:    "peer_list",
		PeerIDs: peerIDs,
	}
	plData, _ := json.Marshal(plMsg)

	msg := SignalMessage{
		Type:       "peer_list",
		Data:       string(plData),
		SenderId:   m.selfID,
		ReceiverId: targetPeerID,
	}
	data, _ := json.Marshal(msg)
	targetPeer.sendSignalRelay(data)

	logger.Debug("Sent peer list to " + targetPeerID + ": " + string(plData))
}

func (m *RTCManager) handlePeerList(peerIDs []string) {
	for _, peerID := range peerIDs {
		if peerID == m.selfID {
			continue
		}

		m.mu.RLock()
		_, exists := m.peers[peerID]
		m.mu.RUnlock()

		if !exists {
			logger.Debug("Discovered new peer from peer_list: " + peerID)
			m.requestPeerConnection(peerID)
		}
	}
}

func (m *RTCManager) requestPeerConnection(peerID string) {
	m.sendSignal(SignalMessage{
		Type:       "Request",
		SenderId:   m.selfID,
		ReceiverId: peerID,
		RoomId:     m.roomID,
		Role:       m.selfRole,
	})
}

func (m *RTCManager) requestAllPeers() {
	m.sig.Send(signal.Message{
		Type:     "Request",
		SenderId: m.selfID,
		RoomId:   m.roomID,
		Role:     string(m.selfRole),
	})
}

func (m *RTCManager) OnChatMessage(fn func(peerID string, msg string)) {
	m.globalChatCallbacks = append(m.globalChatCallbacks, fn)
}

func (m *RTCManager) OnTunnelMessage(fn func(peerID string, msg []byte)) {
	m.globalTunnelMessageCallbacks = append(m.globalTunnelMessageCallbacks, fn)
}

func (m *RTCManager) OnTunnelOpen(fn func(peerID string)) {
	m.globalTunnelOpenCallbacks = append(m.globalTunnelOpenCallbacks, fn)
}

func (m *RTCManager) OnTunnelClose(fn func(peerID string)) {
	m.globalTunnelCloseCallbacks = append(m.globalTunnelCloseCallbacks, fn)
}

func (m *RTCManager) OnPeerConnected(fn func(peerID string)) {
	m.globalPeerConnectedCallbacks = append(m.globalPeerConnectedCallbacks, fn)
}

func (m *RTCManager) SendChatToAll(msg string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, peer := range m.peers {
		peer.SendChat(msg)
	}
}

func (m *RTCManager) SendChatTo(peerID string, msg string) error {
	m.mu.RLock()
	peer := m.peers[peerID]
	m.mu.RUnlock()

	if peer == nil {
		return nil
	}
	return peer.SendChat(msg)
}

func (m *RTCManager) Close() {
	m.mu.Lock()
	peers := make([]*RemotePeer, 0, len(m.peers))
	for _, p := range m.peers {
		peers = append(peers, p)
	}
	m.peers = make(map[string]*RemotePeer)
	m.mu.Unlock()

	for _, p := range peers {
		p.Close()
	}
}
