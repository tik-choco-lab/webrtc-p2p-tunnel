package tcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"

	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
)

const (
	tunnelReadyTimeout = 30 * time.Second
	tcpBufferSize      = 4096
	retryInterval      = 50 * time.Millisecond
)

type manager struct {
	rtcManager *rtc.RTCManager
	mu         sync.RWMutex
	conns      map[string]*tunnelConn
	remoteAddr string
}

type tunnelConn struct {
	conn         net.Conn
	peerID       string
	notifyRemote bool
}

func ListenAndServe(rtcManager *rtc.RTCManager, listenPort int, remoteAddr string) error {
	mgr := newManager(rtcManager)
	return mgr.serve(listenPort, remoteAddr)
}

func newManager(rtcManager *rtc.RTCManager) *manager {
	m := &manager{
		rtcManager: rtcManager,
		conns:      make(map[string]*tunnelConn),
	}

	rtcManager.OnTunnelMessage(func(peerID string, data []byte) {
		m.onTunnelMessage(peerID, data)
	})

	rtcManager.OnTunnelClose(func(peerID string) {
		logger.Debug("tunnel data channel closed for peer " + peerID + "; closing related TCP connections")
		m.closeAllForPeer(peerID)
	})

	return m
}

func (m *manager) serve(listenPort int, remoteAddr string) error {
	if remoteAddr != "" {
		if _, err := net.ResolveTCPAddr("tcp", remoteAddr); err != nil {
			if _, portErr := net.LookupPort("tcp", remoteAddr); portErr == nil {
				remoteAddr = "127.0.0.1:" + remoteAddr
			}
		}
	}
	m.remoteAddr = remoteAddr
	if listenPort == -1 {
		logger.Debug("No local listen port specified; skipping TCP server")
		return nil
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", listenPort))
	if err != nil {
		return err
	}
	logger.Debug("TCP server listening on port " + fmt.Sprint(listenPort))

	for {
		conn, err := ln.Accept()
		if err != nil {
			logger.Error("failed to accept tcp connection: " + err.Error())
			continue
		}
		go func() {
			m.handleLocalConnection(conn)
		}()
	}
}

func (m *manager) handleLocalConnection(conn net.Conn) {
	connID := uuid.NewString()

	peerID, err := m.waitForTunnelReady(tunnelReadyTimeout)
	if err != nil {
		logger.Error("tunnel not ready: " + err.Error())
		conn.Close()
		return
	}

	m.trackConn(connID, conn, peerID, true)

	connectMsg := rtc.TunnelMessage{
		Type:   "connect",
		ConnID: connID,
	}
	if err := m.sendTo(peerID, connectMsg); err != nil {
		logger.Error("failed to send tunnel connect message: " + err.Error())
		m.closeConn(connID, false)
		return
	}

	go m.forwardTCPToDC(connID)
}

func (m *manager) waitForTunnelReady(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		serverPeers := m.rtcManager.GetServerPeers()
		for _, peer := range serverPeers {
			dc := peer.DataChannelTunnel()
			if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				logger.Debug("Selected server peer: " + peer.PeerID())
				return peer.PeerID(), nil
			}
		}

		peers := m.rtcManager.GetAllPeers()
		for _, peer := range peers {
			dc := peer.DataChannelTunnel()
			if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				logger.Debug("Selected peer (no server available): " + peer.PeerID())
				return peer.PeerID(), nil
			}
		}

		if time.Now().After(deadline) {
			return "", errors.New("tunnel data channel not open")
		}
		time.Sleep(retryInterval)
	}
}

func (m *manager) forwardTCPToDC(connID string) {
	tc, ok := m.getConn(connID)
	if !ok {
		return
	}
	buf := make([]byte, tcpBufferSize)
	for {
		n, err := tc.conn.Read(buf)
		if n > 0 {
			payload := append([]byte(nil), buf[:n]...)
			if errSend := m.sendTo(tc.peerID, rtc.TunnelMessage{
				Type:    "data",
				ConnID:  connID,
				Payload: payload,
			}); errSend != nil {
				logger.Error("failed to send tunnel data: " + errSend.Error())
				m.closeConn(connID, false)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Error("tcp read error: " + err.Error())
			}
			m.closeConn(connID, true)
			return
		}
	}
}

func (m *manager) onTunnelMessage(peerID string, data []byte) {
	var tm rtc.TunnelMessage
	if err := json.Unmarshal(data, &tm); err != nil {
		logger.Error("failed to decode tunnel message: " + err.Error())
		return
	}
	switch tm.Type {
	case "connect":
		m.handleRemoteConnect(peerID, tm)
	case "data":
		m.handleRemoteData(tm)
	case "close":
		m.closeConn(tm.ConnID, false)
	default:
		logger.Debug("unknown tunnel message type: " + tm.Type)
	}
}

func (m *manager) handleRemoteConnect(peerID string, tm rtc.TunnelMessage) {
	addr := m.remoteAddr
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		logger.Error("failed to connect to remote target (" + addr + "): " + err.Error())
		_ = m.sendTo(peerID, rtc.TunnelMessage{
			Type:   "close",
			ConnID: tm.ConnID,
		})
		return
	}
	m.trackConn(tm.ConnID, conn, peerID, true)
	go m.forwardTCPToDC(tm.ConnID)
}

func (m *manager) handleRemoteData(tm rtc.TunnelMessage) {
	if len(tm.Payload) == 0 {
		return
	}
	tc, ok := m.getConn(tm.ConnID)
	if !ok {
		logger.Debug("received data for unknown connection: " + tm.ConnID)
		return
	}
	if _, err := tc.conn.Write(tm.Payload); err != nil {
		logger.Error("failed to write to tcp connection: " + err.Error())
		m.closeConn(tm.ConnID, true)
	}
}

func (m *manager) trackConn(connID string, conn net.Conn, peerID string, notifyRemote bool) {
	m.mu.Lock()
	m.conns[connID] = &tunnelConn{conn: conn, peerID: peerID, notifyRemote: notifyRemote}
	m.mu.Unlock()
}

func (m *manager) getConn(connID string) (*tunnelConn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tc, ok := m.conns[connID]
	return tc, ok
}

func (m *manager) closeConn(connID string, notifyRemote bool) {
	m.mu.Lock()
	tc, ok := m.conns[connID]
	if ok {
		if !notifyRemote {
			tc.notifyRemote = false
		}
		delete(m.conns, connID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	_ = tc.conn.Close()
	if notifyRemote && tc.notifyRemote {
		if err := m.sendTo(tc.peerID, rtc.TunnelMessage{Type: "close", ConnID: connID}); err != nil {
			logger.Error("failed to send tunnel close message: " + err.Error())
		}
	}
}

func (m *manager) sendTo(peerID string, msg rtc.TunnelMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	peer := m.rtcManager.GetPeer(peerID)
	if peer == nil {
		return errors.New("peer not found: " + peerID)
	}
	dc := peer.DataChannelTunnel()
	if dc == nil {
		return errors.New("tunnel data channel not ready")
	}
	return dc.Send(data)
}

func (m *manager) closeAllForPeer(peerID string) {
	m.mu.Lock()
	var toClose []*tunnelConn
	for id, tc := range m.conns {
		if tc.peerID == peerID {
			tc.notifyRemote = false
			toClose = append(toClose, tc)
			delete(m.conns, id)
		}
	}
	m.mu.Unlock()
	for _, tc := range toClose {
		_ = tc.conn.Close()
	}
}
