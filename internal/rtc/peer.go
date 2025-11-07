package rtc

import (
	"encoding/json"
	"fmt"

	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"

	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"

	"github.com/pion/webrtc/v3"
)

type Peer struct {
	pc                     *webrtc.PeerConnection
	sig                    *signal.Client
	selfID                 string
	peerID                 string
	roomID                 string
	dcTunnel               *webrtc.DataChannel
	dcChat                 *webrtc.DataChannel
	chatCallbacks          []func(string)
	tunnelMessageCallbacks []func(webrtc.DataChannelMessage)
	tunnelOpenCallbacks    []func()
	tunnelCloseCallbacks   []func()
}

func NewPeer(sig *signal.Client, selfID, roomID string) *Peer {
	p := &Peer{sig: sig, selfID: selfID, roomID: roomID}
	sig.OnMessage(p.handleSignal)
	logger.Debug("created Peer: " + selfID)
	return p
}

func (p *Peer) handleSignal(msg signal.Message) {
	logger.Debug("Received signal message: " + msg.Type + " from " + msg.SenderId)
	switch msg.Type {
	case "Request":
		p.peerID = msg.SenderId
		if p.selfID < p.peerID {
			p.startOffer()
		}
	case "offer":
		p.peerID = msg.SenderId
		p.handleOffer(msg.Data)
	case "answer":
		p.handleAnswer(msg.Data)
	case "candidate":
		p.handleCandidate(msg.Data)
	}
}

func (p *Peer) newPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	logger.Debug(fmt.Sprintf("Using ICE servers: %+v", config.ICEServers))

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	pc.OnICEGatheringStateChange(func(s webrtc.ICEGathererState) {
		logger.Debug("[ICE-Gathering] " + s.String())
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		logger.Debug("[ICE-Connection] " + s.String())
	})

	pc.OnSignalingStateChange(func(s webrtc.SignalingState) {
		logger.Debug("[Signaling] " + s.String())
	})

	return pc, nil
}

func (p *Peer) startOffer() {
	pc, _ := p.newPeerConnection()
	p.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		p.sig.Send(signal.Message{
			Type:       "candidate",
			Data:       string(data),
			SenderId:   p.selfID,
			ReceiverId: p.peerID,
			RoomId:     p.roomID,
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logger.Debug("PeerConnection state: " + s.String())
	})

	dcTunnel, _ := pc.CreateDataChannel("tunnel", nil)
	p.initTunnelDC(dcTunnel)

	p.dcChat, _ = pc.CreateDataChannel("chat", nil)
	p.initChatDC(p.dcChat)

	offer, _ := pc.CreateOffer(nil)
	pc.SetLocalDescription(offer)

	data, _ := json.Marshal(offer)
	p.sig.Send(signal.Message{
		Type:       "offer",
		Data:       string(data),
		SenderId:   p.selfID,
		ReceiverId: p.peerID,
		RoomId:     p.roomID,
	})
}

func (p *Peer) initChatDC(dc *webrtc.DataChannel) {
	p.dcChat = dc

	dc.OnOpen(func() { logger.Debug("DataChannel chat open") })
	dc.OnClose(func() { logger.Debug("DataChannel chat closed") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		for _, cb := range p.chatCallbacks {
			cb(string(msg.Data))
		}
	})
}

func (p *Peer) handleOffer(data string) {
	pc, _ := p.newPeerConnection()
	p.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(c.ToJSON())
		p.sig.Send(signal.Message{
			Type:       "candidate",
			Data:       string(data),
			SenderId:   p.selfID,
			ReceiverId: p.peerID,
			RoomId:     p.roomID,
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logger.Debug("PeerConnection state: " + s.String())
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		logger.Debug("Received DataChannel: " + dc.Label())
		if dc.Label() == "chat" {
			p.initChatDC(dc)
		} else if dc.Label() == "tunnel" {
			p.initTunnelDC(dc)
		}
	})

	var offer webrtc.SessionDescription
	json.Unmarshal([]byte(data), &offer)
	pc.SetRemoteDescription(offer)

	answer, _ := pc.CreateAnswer(nil)
	pc.SetLocalDescription(answer)

	ansData, _ := json.Marshal(answer)
	p.sig.Send(signal.Message{
		Type:       "answer",
		Data:       string(ansData),
		SenderId:   p.selfID,
		ReceiverId: p.peerID,
		RoomId:     p.roomID,
	})
}

func (p *Peer) handleAnswer(data string) {
	var answer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(data), &answer); err != nil {
		logger.Debug("unmarshal answer error: " + err.Error())
		return
	}
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		logger.Debug("SetRemoteDescription(answer) error: " + err.Error())
	}
}

func (p *Peer) handleCandidate(data string) {
	logger.Debug("[Remote-Candidate] " + data)
	var cand webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(data), &cand); err != nil {
		logger.Debug("unmarshal candidate error: " + err.Error())
		return
	}
	if err := p.pc.AddICECandidate(cand); err != nil {
		logger.Debug("AddICECandidate error: " + err.Error())
	}
}

func (p *Peer) DataChannelTunnel() *webrtc.DataChannel {
	return p.dcTunnel
}

func (p *Peer) DataChannelChat() *webrtc.DataChannel {
	return p.dcChat
}

func (p *Peer) OnTunnelMessage(fn func(webrtc.DataChannelMessage)) {
	p.tunnelMessageCallbacks = append(p.tunnelMessageCallbacks, fn)
}

func (p *Peer) OnTunnelOpen(fn func()) {
	p.tunnelOpenCallbacks = append(p.tunnelOpenCallbacks, fn)
	if p.dcTunnel != nil && p.dcTunnel.ReadyState() == webrtc.DataChannelStateOpen {
		fn()
	}
}

func (p *Peer) OnTunnelClose(fn func()) {
	p.tunnelCloseCallbacks = append(p.tunnelCloseCallbacks, fn)
}

func (p *Peer) initTunnelDC(dc *webrtc.DataChannel) {
	p.dcTunnel = dc
	dc.OnOpen(func() {
		logger.Debug("DataChannel tunnel open")
		for _, cb := range p.tunnelOpenCallbacks {
			cb()
		}
	})
	dc.OnClose(func() {
		logger.Debug("DataChannel tunnel closed")
		for _, cb := range p.tunnelCloseCallbacks {
			cb()
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		for _, cb := range p.tunnelMessageCallbacks {
			cb(msg)
		}
	})
}

func (p *Peer) SelfID() string {
	return p.selfID
}
