package rtc

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
)

var errDataChannelNotReady = errors.New("data channel not ready")

type RemotePeer struct {
	pc       *webrtc.PeerConnection
	peerID   string
	role     PeerRole
	dcTunnel *webrtc.DataChannel
	dcChat   *webrtc.DataChannel
	dcSignal *webrtc.DataChannel

	manager      *RTCManager
	mu           sync.RWMutex
	reconnecting bool

	chatCallbacks          []func(string)
	tunnelMessageCallbacks []func(webrtc.DataChannelMessage)
	tunnelOpenCallbacks    []func()
	tunnelCloseCallbacks   []func()
}

func newRemotePeer(manager *RTCManager, peerID string) *RemotePeer {
	return &RemotePeer{
		manager: manager,
		peerID:  peerID,
	}
}

func (rp *RemotePeer) PeerID() string {
	return rp.peerID
}

func (rp *RemotePeer) Role() PeerRole {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.role
}

func (rp *RemotePeer) IsServer() bool {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.role == RoleServer
}

func (rp *RemotePeer) isConnected() bool {
	rp.mu.RLock()
	pc := rp.pc
	rp.mu.RUnlock()

	if pc == nil {
		return false
	}
	state := pc.ConnectionState()
	return state == webrtc.PeerConnectionStateConnected || state == webrtc.PeerConnectionStateConnecting
}

func (rp *RemotePeer) setRole(role PeerRole) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.role = role
}

func (rp *RemotePeer) DataChannelTunnel() *webrtc.DataChannel {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.dcTunnel
}

func (rp *RemotePeer) DataChannelChat() *webrtc.DataChannel {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.dcChat
}

func (rp *RemotePeer) newPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	pc.OnICEGatheringStateChange(func(s webrtc.ICEGathererState) {
		logger.Debug("[" + rp.peerID + "][ICE-Gathering] " + s.String())
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		logger.Debug("[" + rp.peerID + "][ICE-Connection] " + s.String())
	})

	pc.OnSignalingStateChange(func(s webrtc.SignalingState) {
		logger.Debug("[" + rp.peerID + "][Signaling] " + s.String())
	})

	return pc, nil
}

func (rp *RemotePeer) startOffer() error {
	pc, err := rp.newPeerConnection()
	if err != nil {
		return err
	}

	rp.mu.Lock()
	rp.pc = pc
	rp.mu.Unlock()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		rp.manager.sendSignal(SignalMessage{
			Type:       "candidate",
			Data:       string(data),
			SenderId:   rp.manager.selfID,
			ReceiverId: rp.peerID,
			RoomId:     rp.manager.roomID,
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		rp.mu.RLock()
		currentPC := rp.pc
		rp.mu.RUnlock()
		if currentPC != pc {
			return
		}

		logger.Debug("[" + rp.peerID + "] PeerConnection state: " + s.String())

		switch s {
		case webrtc.PeerConnectionStateFailed:
			go rp.handleReconnect()
		case webrtc.PeerConnectionStateDisconnected:
			go func() {
				time.Sleep(3 * time.Second)
				rp.mu.RLock()
				pc := rp.pc
				rp.mu.RUnlock()
				if pc != nil && pc.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					logger.Debug("[" + rp.peerID + "] Still disconnected after timeout, reconnecting...")
					rp.handleReconnect()
				}
			}()
		case webrtc.PeerConnectionStateClosed:
			logger.Debug("[" + rp.peerID + "] PeerConnection closed")
		}
	})

	dcTunnel, err := pc.CreateDataChannel("tunnel", nil)
	if err != nil {
		return err
	}
	rp.initTunnelDC(dcTunnel)

	dcChat, err := pc.CreateDataChannel("chat", nil)
	if err != nil {
		return err
	}
	rp.initChatDC(dcChat)

	dcSignal, err := pc.CreateDataChannel("signal", nil)
	if err != nil {
		return err
	}
	rp.initSignalDC(dcSignal)

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}

	data, _ := json.Marshal(offer)
	rp.manager.sendSignal(SignalMessage{
		Type:       "offer",
		Data:       string(data),
		SenderId:   rp.manager.selfID,
		ReceiverId: rp.peerID,
		RoomId:     rp.manager.roomID,
		Role:       rp.manager.selfRole,
	})

	return nil
}

func (rp *RemotePeer) handleOffer(data string) error {
	pc, err := rp.newPeerConnection()
	if err != nil {
		return err
	}

	rp.mu.Lock()
	rp.pc = pc
	rp.mu.Unlock()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		rp.manager.sendSignal(SignalMessage{
			Type:       "candidate",
			Data:       string(data),
			SenderId:   rp.manager.selfID,
			ReceiverId: rp.peerID,
			RoomId:     rp.manager.roomID,
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		rp.mu.RLock()
		currentPC := rp.pc
		rp.mu.RUnlock()
		if currentPC != pc {
			return
		}

		logger.Debug("[" + rp.peerID + "] PeerConnection state: " + s.String())

		switch s {
		case webrtc.PeerConnectionStateFailed:
			go rp.handleReconnect()
		case webrtc.PeerConnectionStateDisconnected:
			go func() {
				time.Sleep(3 * time.Second)
				rp.mu.RLock()
				pc := rp.pc
				rp.mu.RUnlock()
				if pc != nil && pc.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					logger.Debug("[" + rp.peerID + "] Still disconnected after timeout, reconnecting...")
					rp.handleReconnect()
				}
			}()
		case webrtc.PeerConnectionStateClosed:
			logger.Debug("[" + rp.peerID + "] PeerConnection closed")
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		logger.Debug("[" + rp.peerID + "] Received DataChannel: " + dc.Label())
		switch dc.Label() {
		case "chat":
			rp.initChatDC(dc)
		case "tunnel":
			rp.initTunnelDC(dc)
		case "signal":
			rp.initSignalDC(dc)
		}
	})

	var offer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(data), &offer); err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}

	ansData, _ := json.Marshal(answer)
	rp.manager.sendSignal(SignalMessage{
		Type:       "answer",
		Data:       string(ansData),
		SenderId:   rp.manager.selfID,
		ReceiverId: rp.peerID,
		RoomId:     rp.manager.roomID,
		Role:       rp.manager.selfRole,
	})

	return nil
}

func (rp *RemotePeer) handleAnswer(data string) error {
	var answer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(data), &answer); err != nil {
		logger.Debug("[" + rp.peerID + "] unmarshal answer error: " + err.Error())
		return err
	}

	rp.mu.RLock()
	pc := rp.pc
	rp.mu.RUnlock()

	if pc == nil {
		logger.Debug("[" + rp.peerID + "] no peer connection for answer")
		return nil
	}

	if err := pc.SetRemoteDescription(answer); err != nil {
		logger.Debug("[" + rp.peerID + "] SetRemoteDescription(answer) error: " + err.Error())
		return err
	}
	return nil
}

func (rp *RemotePeer) handleCandidate(data string) error {
	logger.Debug("[" + rp.peerID + "][Remote-Candidate] " + data)
	var cand webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(data), &cand); err != nil {
		logger.Debug("[" + rp.peerID + "] unmarshal candidate error: " + err.Error())
		return err
	}

	rp.mu.RLock()
	pc := rp.pc
	rp.mu.RUnlock()

	if pc == nil {
		logger.Debug("[" + rp.peerID + "] no peer connection for candidate")
		return nil
	}

	if err := pc.AddICECandidate(cand); err != nil {
		logger.Debug("[" + rp.peerID + "] AddICECandidate error: " + err.Error())
		return err
	}
	return nil
}

func (rp *RemotePeer) initChatDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcChat = dc
	rp.mu.Unlock()

	dc.OnOpen(func() { logger.Debug("[" + rp.peerID + "] DataChannel chat open") })
	dc.OnClose(func() { logger.Debug("[" + rp.peerID + "] DataChannel chat closed") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		for _, cb := range rp.chatCallbacks {
			cb(string(msg.Data))
		}
	})
}

func (rp *RemotePeer) initTunnelDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcTunnel = dc
	rp.mu.Unlock()

	dc.OnOpen(func() {
		logger.Debug("[" + rp.peerID + "] DataChannel tunnel open")
		for _, cb := range rp.tunnelOpenCallbacks {
			cb()
		}
	})
	dc.OnClose(func() {
		logger.Debug("[" + rp.peerID + "] DataChannel tunnel closed")
		for _, cb := range rp.tunnelCloseCallbacks {
			cb()
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		for _, cb := range rp.tunnelMessageCallbacks {
			cb(msg)
		}
	})
}

func (rp *RemotePeer) initSignalDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcSignal = dc
	rp.mu.Unlock()

	dc.OnOpen(func() {
		logger.Debug("[" + rp.peerID + "] DataChannel signal open")
		rp.manager.broadcastPeerList(rp.peerID)
	})
	dc.OnClose(func() {
		logger.Debug("[" + rp.peerID + "] DataChannel signal closed")
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		rp.manager.handleRelayedSignal(msg.Data)
	})
}

func (rp *RemotePeer) handleReconnect() {
	rp.mu.Lock()
	if rp.reconnecting {
		rp.mu.Unlock()
		return
	}
	rp.reconnecting = true
	pc := rp.pc
	rp.pc = nil
	rp.dcTunnel = nil
	rp.dcChat = nil
	rp.dcSignal = nil
	rp.mu.Unlock()

	defer func() {
		rp.mu.Lock()
		rp.reconnecting = false
		rp.mu.Unlock()
	}()

	if pc != nil {
		pc.Close()
	}

	time.Sleep(1 * time.Second)

	logger.Debug("[" + rp.peerID + "] Re-requesting peer connection...")
	rp.manager.requestPeerConnection(rp.peerID)
}

func (rp *RemotePeer) Close() {
	rp.mu.Lock()
	pc := rp.pc
	rp.pc = nil
	rp.dcTunnel = nil
	rp.dcChat = nil
	rp.dcSignal = nil
	rp.mu.Unlock()

	if pc != nil {
		pc.Close()
	}
}

func (rp *RemotePeer) OnChatMessage(fn func(string)) {
	rp.chatCallbacks = append(rp.chatCallbacks, fn)
}

func (rp *RemotePeer) OnTunnelMessage(fn func(webrtc.DataChannelMessage)) {
	rp.tunnelMessageCallbacks = append(rp.tunnelMessageCallbacks, fn)
}

func (rp *RemotePeer) OnTunnelOpen(fn func()) {
	rp.tunnelOpenCallbacks = append(rp.tunnelOpenCallbacks, fn)
	rp.mu.RLock()
	dc := rp.dcTunnel
	rp.mu.RUnlock()
	if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
		fn()
	}
}

func (rp *RemotePeer) OnTunnelClose(fn func()) {
	rp.tunnelCloseCallbacks = append(rp.tunnelCloseCallbacks, fn)
}

func (rp *RemotePeer) SendChat(msg string) error {
	rp.mu.RLock()
	dc := rp.dcChat
	rp.mu.RUnlock()

	if dc == nil {
		return nil
	}
	return dc.SendText(msg)
}

func (rp *RemotePeer) sendSignalRelay(data []byte) error {
	rp.mu.RLock()
	dc := rp.dcSignal
	rp.mu.RUnlock()

	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return errDataChannelNotReady
	}
	return dc.Send(data)
}
