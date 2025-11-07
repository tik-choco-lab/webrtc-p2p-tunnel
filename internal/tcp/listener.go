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
)

type manager struct {
	peer       *rtc.Peer
	mu         sync.RWMutex
	conns      map[string]*tunnelConn
	remotePort int
}

type tunnelConn struct {
	conn         net.Conn
	notifyRemote bool
}

func ListenAndServe(peer *rtc.Peer, listenPort int, remotePort int) error {
	mgr := newManager(peer)
	return mgr.serve(listenPort, remotePort)
}

func newManager(peer *rtc.Peer) *manager {
	m := &manager{
		peer:  peer,
		conns: make(map[string]*tunnelConn),
	}

	peer.OnTunnelMessage(m.onTunnelMessage)
	peer.OnTunnelClose(func() {
		logger.Debug("tunnel data channel closed; closing active TCP connections")
		m.closeAll()
	})
	return m
}

func (m *manager) serve(listenPort int, remotePort int) error {
	m.remotePort = remotePort
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
		go m.handleLocalConnection(conn, remotePort)
	}
}

func (m *manager) handleLocalConnection(conn net.Conn, remotePort int) {
	connID := uuid.NewString()
	m.trackConn(connID, conn, true)

	if err := m.waitForTunnelReady(tunnelReadyTimeout); err != nil {
		logger.Error("tunnel not ready: " + err.Error())
		m.closeConn(connID, false)
		return
	}

	connectMsg := rtc.TunnelMessage{
		Type:   "connect",
		ConnID: connID,
	}
	if err := m.send(connectMsg); err != nil {
		logger.Error("failed to send tunnel connect message: " + err.Error())
		m.closeConn(connID, false)
		return
	}

	go m.forwardTCPToDC(connID)
}

func (m *manager) waitForTunnelReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		dc := m.peer.DataChannelTunnel()
		if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("tunnel data channel not open")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *manager) forwardTCPToDC(connID string) {
	tc, ok := m.getConn(connID)
	if !ok {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := tc.conn.Read(buf)
		if n > 0 {
			payload := append([]byte(nil), buf[:n]...)
			if errSend := m.send(rtc.TunnelMessage{
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

func (m *manager) onTunnelMessage(msg webrtc.DataChannelMessage) {
	var tm rtc.TunnelMessage
	if err := json.Unmarshal(msg.Data, &tm); err != nil {
		logger.Error("failed to decode tunnel message: " + err.Error())
		return
	}
	switch tm.Type {
	case "connect":
		m.handleRemoteConnect(tm)
	case "data":
		m.handleRemoteData(tm)
	case "close":
		m.closeConn(tm.ConnID, false)
	default:
		logger.Debug("unknown tunnel message type: " + tm.Type)
	}
}

func (m *manager) handleRemoteConnect(tm rtc.TunnelMessage) {
	addr := fmt.Sprintf("127.0.0.1:%d", m.remotePort)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		logger.Error("failed to connect to remote target: " + err.Error())
		_ = m.send(rtc.TunnelMessage{
			Type:   "close",
			ConnID: tm.ConnID,
		})
		return
	}
	m.trackConn(tm.ConnID, conn, true)
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

func (m *manager) trackConn(connID string, conn net.Conn, notifyRemote bool) {
	m.mu.Lock()
	m.conns[connID] = &tunnelConn{conn: conn, notifyRemote: notifyRemote}
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
		if err := m.send(rtc.TunnelMessage{Type: "close", ConnID: connID}); err != nil {
			logger.Error("failed to send tunnel close message: " + err.Error())
		}
	}
}

func (m *manager) closeAll() {
	m.mu.Lock()
	conns := make([]*tunnelConn, 0, len(m.conns))
	for id, tc := range m.conns {
		tc.notifyRemote = false
		conns = append(conns, tc)
		delete(m.conns, id)
	}
	m.mu.Unlock()
	for _, tc := range conns {
		_ = tc.conn.Close()
	}
}

func (m *manager) send(msg rtc.TunnelMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	dc := m.peer.DataChannelTunnel()
	if dc == nil {
		return errors.New("tunnel data channel not ready")
	}
	return dc.Send(data)
}
