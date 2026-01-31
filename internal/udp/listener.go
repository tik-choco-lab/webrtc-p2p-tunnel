package udp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
)

const (
	udpTimeout         = 30 * time.Second
	tunnelReadyTimeout = 30 * time.Second
	maxUDPSize         = 65535
	cleanupInterval    = 10 * time.Second
	retryInterval      = 100 * time.Millisecond
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
	buf := make([]byte, maxUDPSize)
	for {
		n, addr, err := m.localConn.ReadFrom(buf)
		if err != nil {
			logger.Error("UDP read error: " + err.Error())
			return
		}

		connID := addr.String()
		m.mu.Lock()
		uc, ok := m.conns[connID]
		if !ok {
			peerID, err := m.waitForTunnelReady(tunnelReadyTimeout)
			if err != nil {
				logger.Error("Tunnel not ready for UDP: " + err.Error())
				m.mu.Unlock()
				continue
			}
			uc = &udpConn{
				peerID:     peerID,
				lastSeen:   time.Now(),
				clientAddr: addr,
			}
			m.conns[connID] = uc
		}
		uc.lastSeen = time.Now()
		peerID := uc.peerID
		m.mu.Unlock()

		payload := append([]byte(nil), buf[:n]...)
		m.sendTo(peerID, rtc.TunnelMessage{
			Type:    "data",
			ConnID:  connID,
			Payload: payload,
		})
	}
}

func (m *manager) onTunnelMessage(peerID string, data []byte) {
	var tm rtc.TunnelMessage
	if err := json.Unmarshal(data, &tm); err != nil {
		return
	}

	if tm.Type == "data" {
		m.handleData(peerID, tm)
	}
}

func (m *manager) handleData(peerID string, tm rtc.TunnelMessage) {
	m.mu.Lock()
	uc, ok := m.conns[tm.ConnID]
	if !ok {
		if m.remoteAddr != "" {
			conn, err := net.Dial("udp", m.remoteAddr)
			if err != nil {
				logger.Error("Failed to dial UDP target: " + err.Error())
				m.mu.Unlock()
				return
			}
			uc = &udpConn{
				targetConn: conn,
				peerID:     peerID,
				lastSeen:   time.Now(),
			}
			m.conns[tm.ConnID] = uc
			go m.forwardTargetToTunnel(tm.ConnID, conn, peerID)
		} else if m.localConn != nil {
			addr, err := net.ResolveUDPAddr("udp", tm.ConnID)
			if err == nil {
				m.localConn.WriteTo(tm.Payload, addr)
			}
			m.mu.Unlock()
			return
		}
	}

	if uc != nil {
		uc.lastSeen = time.Now()
		if uc.targetConn != nil {
			uc.targetConn.Write(tm.Payload)
		} else if m.localConn != nil && uc.clientAddr != nil {
			m.localConn.WriteTo(tm.Payload, uc.clientAddr)
		}
	}
	m.mu.Unlock()
}

func (m *manager) forwardTargetToTunnel(connID string, conn net.Conn, peerID string) {
	buf := make([]byte, maxUDPSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			payload := append([]byte(nil), buf[:n]...)
			m.sendTo(peerID, rtc.TunnelMessage{
				Type:    "data",
				ConnID:  connID,
				Payload: payload,
			})
		}
		if err != nil {
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
		time.Sleep(retryInterval)
	}
}

func (m *manager) cleanup() {
	ticker := time.NewTicker(cleanupInterval)
	for range ticker.C {
		m.mu.Lock()
		for id, uc := range m.conns {
			if time.Since(uc.lastSeen) > udpTimeout {
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
