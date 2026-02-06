package rtc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/auth"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
)

const (
	ReconnectDelay    = 1 * time.Second
	PeerIDPrefixLen   = 4
	DefaultSTUNServer = "stun:stun.l.google.com:19302"

	DCLabelTunnel = "tunnel"
	DCLabelChat   = "chat"
	DCLabelSignal = "signal"
	DCLabelStdio  = "stdio"
	DCLabelAuth   = "auth"

	LogTruncateLen = 8
	MsgIDFormat    = "%s_%s_%d"
	TimeFormat     = "150405.000"
)

var (
	errDataChannelNotReady = errors.New("data channel not ready")
	msgSeq                 uint64
)

type RemotePeer struct {
	pc       *webrtc.PeerConnection
	peerID   string
	role     PeerRole
	dcTunnel *webrtc.DataChannel
	dcChat   *webrtc.DataChannel
	dcSignal *webrtc.DataChannel
	dcStdio  *webrtc.DataChannel
	dcAuth   *webrtc.DataChannel

	manager       *RTCManager
	mu            sync.RWMutex
	reconnecting  bool
	authenticated bool
	lastChallenge string
}

func newRemotePeer(manager *RTCManager, peerID string) *RemotePeer {
	return &RemotePeer{manager: manager, peerID: peerID}
}

func (rp *RemotePeer) PeerID() string        { return rp.peerID }
func (rp *RemotePeer) Role() PeerRole        { rp.mu.RLock(); defer rp.mu.RUnlock(); return rp.role }
func (rp *RemotePeer) IsServer() bool        { return rp.Role() == RoleServer }
func (rp *RemotePeer) setRole(role PeerRole) { rp.mu.Lock(); defer rp.mu.Unlock(); rp.role = role }

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
func (rp *RemotePeer) DataChannelStdio() *webrtc.DataChannel {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.dcStdio
}

func (rp *RemotePeer) newPeerConnection() (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{DefaultSTUNServer}}},
	})
	if err != nil {
		return nil, err
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		rp.manager.sendSignal(signal.Message{
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
		logger.Debug("[" + rp.peerID + "] PeerConn: " + s.String())
		if s == webrtc.PeerConnectionStateFailed {
			go rp.handleReconnect()
		}
	})

	return pc, nil
}

func (rp *RemotePeer) startOffer() error {
	if pc := rp.teardown(); pc != nil {
		pc.Close()
	}
	pc, err := rp.newPeerConnection()
	if err != nil {
		return err
	}
	rp.mu.Lock()
	rp.pc = pc
	rp.authenticated = rp.manager.authorizer == nil
	rp.mu.Unlock()

	for _, label := range []string{DCLabelTunnel, DCLabelChat, DCLabelSignal, DCLabelStdio, DCLabelAuth} {
		dc, err := pc.CreateDataChannel(label, nil)
		if err != nil {
			return err
		}
		rp.initDCByLabel(dc)
	}

	offer, _ := pc.CreateOffer(nil)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	data, _ := json.Marshal(offer)
	rp.manager.sendSignal(signal.Message{
		Type:       "offer",
		Data:       string(data),
		SenderId:   rp.manager.selfID,
		ReceiverId: rp.peerID,
		RoomId:     rp.manager.roomID,
		Role:       string(rp.manager.selfRole),
	})
	return nil
}

func (rp *RemotePeer) handleOffer(data string) error {
	if pc := rp.teardown(); pc != nil {
		pc.Close()
	}
	pc, err := rp.newPeerConnection()
	if err != nil {
		return err
	}
	rp.mu.Lock()
	rp.pc = pc
	rp.authenticated = rp.manager.authorizer == nil
	rp.mu.Unlock()

	pc.OnDataChannel(rp.initDCByLabel)

	var offer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(data), &offer); err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return err
	}
	answer, _ := pc.CreateAnswer(nil)
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}

	ansData, _ := json.Marshal(answer)
	rp.manager.sendSignal(signal.Message{
		Type:       "answer",
		Data:       string(ansData),
		SenderId:   rp.manager.selfID,
		ReceiverId: rp.peerID,
		RoomId:     rp.manager.roomID,
		Role:       string(rp.manager.selfRole),
	})
	return nil
}

func (rp *RemotePeer) handleAnswer(data string) error {
	rp.mu.RLock()
	pc := rp.pc
	rp.mu.RUnlock()
	if pc == nil {
		return nil
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(data), &answer); err != nil {
		return err
	}
	return pc.SetRemoteDescription(answer)
}

func (rp *RemotePeer) handleCandidate(data string) error {
	rp.mu.RLock()
	pc := rp.pc
	rp.mu.RUnlock()
	if pc == nil {
		return nil
	}
	var cand webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(data), &cand); err != nil {
		return err
	}
	return pc.AddICECandidate(cand)
}

func (rp *RemotePeer) initDCByLabel(dc *webrtc.DataChannel) {
	inits := map[string]func(*webrtc.DataChannel){
		DCLabelTunnel: rp.initTunnelDC,
		DCLabelChat:   rp.initChatDC,
		DCLabelSignal: rp.initSignalDC,
		DCLabelStdio:  rp.initStdioDC,
		DCLabelAuth:   rp.initAuthDC,
	}
	if init, ok := inits[dc.Label()]; ok {
		init(dc)
	}
}

func (rp *RemotePeer) initChatDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcChat = dc
	rp.mu.Unlock()
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if rp.isAuthorized() {
			rp.manager.notifyChat(rp.peerID, string(msg.Data))
		}
	})
}

func (rp *RemotePeer) initTunnelDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcTunnel = dc
	rp.mu.Unlock()
	dc.OnOpen(func() {
		if rp.isAuthorized() {
			rp.manager.notifyTunnelOpen(rp.peerID)
		}
	})
	dc.OnClose(func() { rp.manager.notifyTunnelClose(rp.peerID) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if rp.isAuthorized() {
			rp.manager.notifyTunnelMsg(rp.peerID, msg.Data)
		}
	})
}

func (rp *RemotePeer) initSignalDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcSignal = dc
	rp.mu.Unlock()
	dc.OnOpen(func() {
		rp.manager.broadcastPeerList(rp.peerID)
		rp.manager.notifyPeerConnected(rp.peerID)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		rp.manager.handleRawRelay(msg.Data)
	})
}

func (rp *RemotePeer) initStdioDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcStdio = dc
	rp.mu.Unlock()
	dc.OnOpen(func() {
		if rp.isAuthorized() {
			rp.manager.notifyStdioOpen(rp.peerID)
		}
	})
	dc.OnClose(func() { rp.manager.notifyStdioClose(rp.peerID) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if rp.isAuthorized() {
			rp.manager.notifyStdioMsg(rp.peerID, msg.Data)
		}
	})
}

func (rp *RemotePeer) initAuthDC(dc *webrtc.DataChannel) {
	rp.mu.Lock()
	rp.dcAuth = dc
	rp.mu.Unlock()

	dc.OnOpen(func() {
		if rp.manager.authorizer != nil {
			nonce, _ := auth.NewChallenge()
			rp.mu.Lock()
			rp.lastChallenge = nonce
			rp.mu.Unlock()
			msg, _ := json.Marshal(auth.Message{Type: "challenge", Nonce: nonce})
			dc.Send(msg)
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var authMsg auth.Message
		if err := json.Unmarshal(msg.Data, &authMsg); err != nil {
			return
		}

		handlers := map[string]func(){
			auth.AuthMsgTypeChallenge: func() { rp.handleAuthChallenge(dc, authMsg) },
			auth.AuthMsgTypeResponse:  func() { rp.handleAuthResponse(dc, authMsg) },
			auth.AuthMsgTypeSuccess:   func() { rp.handleAuthSuccess() },
			auth.AuthMsgTypeError:     func() { rp.handleAuthError(authMsg) },
		}
		if handler, ok := handlers[authMsg.Type]; ok {
			handler()
		}
	})
}

func (rp *RemotePeer) handleAuthChallenge(dc *webrtc.DataChannel, authMsg auth.Message) {
	if rp.manager.authenticator == nil {
		return
	}
	logger.Debug("[" + rp.peerID + "] Auth: Received challenge, signing response")

	nonceBytes, err := base64.StdEncoding.DecodeString(authMsg.Nonce)
	if err != nil {
		logger.Error("[" + rp.peerID + "] Auth: Failed to decode nonce: " + err.Error())
		return
	}

	sig, err := rp.manager.authenticator.Sign(nonceBytes)
	if err != nil {
		logger.Error("[" + rp.peerID + "] Auth: Failed to sign nonce: " + err.Error())
		return
	}

	pubKey := rp.manager.authenticator.PublicKey()
	identity := base64.StdEncoding.EncodeToString(pubKey)
	sigStr := base64.StdEncoding.EncodeToString(sig)
	sigShort := sigStr
	if len(sigShort) > LogTruncateLen {
		sigShort = sigShort[:LogTruncateLen]
	}

	logger.Debug(fmt.Sprintf("[%s] Auth: Debug Info - PubKey(hex): %x, Nonce(raw): %s, Signature(hex): %s",
		rp.peerID, pubKey, authMsg.Nonce, sigShort+"..."))

	resp, _ := json.Marshal(auth.Message{
		Type:      "response",
		Nonce:     authMsg.Nonce,
		Token:     rp.manager.authenticator.Token(),
		Identity:  identity,
		Signature: sigStr,
	})
	dc.Send(resp)
}

func (rp *RemotePeer) handleAuthResponse(dc *webrtc.DataChannel, authMsg auth.Message) {
	if rp.manager.authorizer == nil {
		return
	}
	pubKey, err := base64.StdEncoding.DecodeString(authMsg.Identity)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] Auth: Failed to decode identity: %v", rp.peerID, err))
		return
	}

	idShort := authMsg.Identity
	if len(idShort) > LogTruncateLen {
		idShort = idShort[:LogTruncateLen]
	}
	logger.Debug("[" + rp.peerID + "] Auth: Received response from " + idShort + "...")

	authorized := auth.Authorize(rp.manager.authorizer, pubKey, authMsg.Token)
	logger.Debug(fmt.Sprintf("[%s] Auth: Authorization result: %v", rp.peerID, authorized))

	sigOk := auth.Verify(pubKey, authMsg.Nonce, authMsg.Signature)
	rp.mu.RLock()
	nonceOk := authMsg.Nonce == rp.lastChallenge
	rp.mu.RUnlock()

	sigShort := authMsg.Signature
	if len(sigShort) > LogTruncateLen {
		sigShort = sigShort[:LogTruncateLen]
	}

	logger.Debug(fmt.Sprintf("[%s] Auth: Debug Info - PubKey(hex): %x, Nonce(raw): %s, Signature(hex): %s",
		rp.peerID, pubKey, authMsg.Nonce, sigShort+"..."))

	logger.Debug(fmt.Sprintf("[%s] Auth: Verify results - Signature: %v, Nonce: %v", rp.peerID, sigOk, nonceOk))

	if sigOk && authorized && nonceOk {
		rp.mu.Lock()
		rp.authenticated = true
		rp.mu.Unlock()
		logger.Info("[" + rp.peerID + "] Authenticated successfully")
		resp, _ := json.Marshal(auth.Message{Type: "success"})
		dc.Send(resp)
		rp.manager.notifyAuth(rp.peerID, true)
		rp.notifyChannelsOnAuth()
	} else {
		logger.Error("[" + rp.peerID + "] Authentication failed")
		resp, _ := json.Marshal(auth.Message{Type: "error", Error: "unauthorized"})
		dc.Send(resp)
		rp.manager.notifyAuth(rp.peerID, false)
		rp.Close()
	}
}

func (rp *RemotePeer) handleAuthSuccess() {
	rp.mu.Lock()
	rp.authenticated = true
	rp.mu.Unlock()
	logger.Info("[" + rp.peerID + "] Server authenticated us")
	logger.Debug("[" + rp.peerID + "] Auth: Success received")
	rp.manager.notifyAuth(rp.peerID, true)
	rp.notifyChannelsOnAuth()
}

func (rp *RemotePeer) handleAuthError(authMsg auth.Message) {
	logger.Error("[" + rp.peerID + "] Server rejected authentication: " + authMsg.Error)
	logger.Debug("[" + rp.peerID + "] Auth: Error received: " + authMsg.Error)
	rp.manager.notifyAuth(rp.peerID, false)
	rp.Close()
}

func (rp *RemotePeer) notifyChannelsOnAuth() {
	if dc := rp.DataChannelTunnel(); dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
		rp.manager.notifyTunnelOpen(rp.peerID)
	}
	if dc := rp.DataChannelStdio(); dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
		rp.manager.notifyStdioOpen(rp.peerID)
	}
}

func (rp *RemotePeer) isAuthorized() bool {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.authenticated
}

func (rp *RemotePeer) teardown() *webrtc.PeerConnection {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.pc != nil {
		logger.Debug("[" + rp.peerID + "] Tearing down connection")
	}
	pc := rp.pc
	rp.pc, rp.dcTunnel, rp.dcChat, rp.dcSignal, rp.dcStdio, rp.dcAuth = nil, nil, nil, nil, nil, nil
	return pc
}

func (rp *RemotePeer) handleReconnect() {
	rp.mu.Lock()
	if rp.reconnecting {
		rp.mu.Unlock()
		return
	}
	rp.reconnecting = true
	rp.mu.Unlock()
	defer func() { rp.mu.Lock(); rp.reconnecting = false; rp.mu.Unlock() }()

	if pc := rp.teardown(); pc != nil {
		pc.Close()
	}
	time.Sleep(ReconnectDelay)
	rp.manager.requestPeerConnection(rp.peerID)
}

func (rp *RemotePeer) Close() {
	if pc := rp.teardown(); pc != nil {
		pc.Close()
	}
}

func (rp *RemotePeer) OnTunnelOpen(fn func()) {
	rp.mu.RLock()
	dc := rp.dcTunnel
	rp.mu.RUnlock()
	if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
		fn()
	}
}

func (rp *RemotePeer) SendChat(msg string) error {
	if !rp.isAuthorized() {
		return errors.New("not authorized")
	}
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

func nextMsgID(selfID string) string {
	seq := atomic.AddUint64(&msgSeq, 1)
	prefix := selfID
	if len(prefix) > PeerIDPrefixLen {
		prefix = prefix[:PeerIDPrefixLen]
	}
	return fmt.Sprintf(MsgIDFormat, time.Now().Format(TimeFormat), prefix, seq)
}
