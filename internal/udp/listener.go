package udp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
)

const (
	UDPTimeout         = 30 * time.Second
	TunnelReadyTimeout = 30 * time.Second
	MaxUDPSize         = 65535
	CleanupInterval    = 10 * time.Second
	RetryInterval      = 100 * time.Millisecond
)

type manager struct {
	rtcManager *rtc.RTCManager
	mu         sync.RWMutex
	conns      map[string]*udpConn
	remoteAddr string
	localConn  net.PacketConn
}

type udpConn struct {
	targetConn net.Conn
	lastSeen   time.Time
	peerID     string
	clientAddr net.Addr
}

func ListenAndServe(rtcManager *rtc.RTCManager, listenPort int, remoteAddr string) error {
	mgr := &manager{
		rtcManager: rtcManager,
		conns:      make(map[string]*udpConn),
		remoteAddr: remoteAddr,
	}

	rtcManager.OnTunnelMessage(func(peerID string, data []byte) {
		mgr.onTunnelMessage(peerID, data)
	})

	return mgr.serve(listenPort)
}

func (m *manager) serve(listenPort int) error {
	if listenPort != -1 {
		pc, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", listenPort))
		if err != nil {
			return err
		}
		m.localConn = pc
		logger.Debug(fmt.Sprintf("UDP server listening on port %d", listenPort))

		go m.readLocalPackets()
	}

	go m.cleanup()

	return nil
}

func (m *manager) readLocalPackets() {
	buf := make([]byte, MaxUDPSize)
	for {
		n, addr, err := m.localConn.ReadFrom(buf)
		if err != nil {
			logger.Error("UDP read error: " + err.Error())
			return
		}

		connID := addr.String()
		m.mu.RLock()
		uc, ok := m.conns[connID]
		m.mu.RUnlock()

		if !ok {
			peerID, err := m.waitForTunnelReady(TunnelReadyTimeout)
			if err != nil {
				logger.Error("Tunnel not ready for UDP: " + err.Error())
				continue
			}

			m.mu.Lock()
			if uc, ok = m.conns[connID]; !ok {
				uc = &udpConn{
					peerID:     peerID,
					lastSeen:   time.Now(),
					clientAddr: addr,
				}
				m.conns[connID] = uc
			}
			m.mu.Unlock()
		}

		m.mu.Lock()
		uc.lastSeen = time.Now()
		pID := uc.peerID
		m.mu.Unlock()

		payload := append([]byte(nil), buf[:n]...)
		if err := m.sendTo(pID, rtc.TunnelMessage{
			Type:    rtc.TunnelMsgTypeData,
			ConnID:  connID,
			Payload: payload,
		}); err != nil {
			logger.Error(fmt.Sprintf("Failed to send UDP data to peer %s: %v", pID, err))
		}
	}
}

func (m *manager) onTunnelMessage(peerID string, data []byte) {
	var tm rtc.TunnelMessage
	if err := json.Unmarshal(data, &tm); err != nil {
		return
	}

	if tm.Type == rtc.TunnelMsgTypeData {
		m.handleData(peerID, tm)
	}
}

func (m *manager) handleData(peerID string, tm rtc.TunnelMessage) {
	m.mu.RLock()
	uc, ok := m.conns[tm.ConnID]
	m.mu.RUnlock()

	if !ok {
		if m.remoteAddr != "" {
			conn, err := net.Dial("udp", m.remoteAddr)
			if err != nil {
				logger.Error("Failed to dial UDP target: " + err.Error())
				return
			}
			m.mu.Lock()
			if uc, ok = m.conns[tm.ConnID]; !ok {
				uc = &udpConn{
					targetConn: conn,
					peerID:     peerID,
					lastSeen:   time.Now(),
				}
				m.conns[tm.ConnID] = uc
				go m.forwardTargetToTunnel(tm.ConnID, conn, peerID)
			} else {
				conn.Close() // Already exists, close this one
			}
			m.mu.Unlock()
		} else if m.localConn != nil {
			addr, err := net.ResolveUDPAddr("udp", tm.ConnID)
			if err == nil {
				if _, err := m.localConn.WriteTo(tm.Payload, addr); err != nil {
					logger.Error("UDP local write error: " + err.Error())
				}
			}
			return
		}
	}

	if uc != nil {
		m.mu.Lock()
		uc.lastSeen = time.Now()
		m.mu.Unlock()

		if uc.targetConn != nil {
			if _, err := uc.targetConn.Write(tm.Payload); err != nil {
				logger.Error("UDP target write error: " + err.Error())
			}
		} else if m.localConn != nil && uc.clientAddr != nil {
			if _, err := m.localConn.WriteTo(tm.Payload, uc.clientAddr); err != nil {
				logger.Error("UDP local write error: " + err.Error())
			}
		}
	}
}

func (m *manager) forwardTargetToTunnel(connID string, conn net.Conn, peerID string) {
	buf := make([]byte, MaxUDPSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			m.mu.Lock()
			if uc, ok := m.conns[connID]; ok {
				uc.lastSeen = time.Now()
			}
			m.mu.Unlock()

			payload := append([]byte(nil), buf[:n]...)
			if errSend := m.sendTo(peerID, rtc.TunnelMessage{
				Type:    rtc.TunnelMsgTypeData,
				ConnID:  connID,
				Payload: payload,
			}); errSend != nil {
				logger.Error(fmt.Sprintf("Failed to send target UDP data to tunnel: %v", errSend))
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				logger.Error("UDP target read error: " + err.Error())
			}
			return
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
		return errors.New("peer not found")
	}
	dc := peer.DataChannelTunnel()
	if dc == nil {
		return errors.New("dc not ready")
	}
	return dc.Send(data)
}

func (m *manager) waitForTunnelReady(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		peers := m.rtcManager.GetServerPeers()
		for _, peer := range peers {
			dc := peer.DataChannelTunnel()
			if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				return peer.PeerID(), nil
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("timeout")
		}
		time.Sleep(RetryInterval)
	}
}

func (m *manager) cleanup() {
	ticker := time.NewTicker(CleanupInterval)
	for range ticker.C {
		m.mu.Lock()
		for id, uc := range m.conns {
			if time.Since(uc.lastSeen) > UDPTimeout {
				if uc.targetConn != nil {
					uc.targetConn.Close()
				}
				delete(m.conns, id)
				logger.Debug("Cleaned up UDP session: " + id)
			}
		}
		m.mu.Unlock()
	}
}
