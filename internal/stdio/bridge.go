package stdio

import (
	"io"
	"os"
	"sync"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
)

type Bridge struct {
	manager    *rtc.RTCManager
	mu         sync.Mutex
	buffer     [][]byte
	connected  bool
	activePeer *rtc.RemotePeer
	done       chan struct{}
}

func NewBridge(manager *rtc.RTCManager) *Bridge {
	return &Bridge{
		manager: manager,
		buffer:  make([][]byte, 0),
		done:    make(chan struct{}),
	}
}

func (b *Bridge) Run() error {
	b.manager.OnTunnelMessage(func(peerID string, data []byte) {
		b.mu.Lock()
		if b.activePeer == nil || b.activePeer.PeerID() != peerID {
			b.activePeer = b.manager.GetPeer(peerID)
		}
		b.mu.Unlock()

		streamType, payload := Unwrap(data)
		switch streamType {
		case StreamStdout:
			os.Stdout.Write(payload)
		case StreamStderr:
			os.Stderr.Write(payload)
		}
	})

	b.manager.OnTunnelOpen(func(peerID string) {
		b.mu.Lock()
		defer b.mu.Unlock()

		if !b.connected {
			b.connected = true
			b.activePeer = b.manager.GetPeer(peerID)
			logger.Debug("stdio bridge connected to peer: " + peerID)

			if len(b.buffer) > 0 {
				logger.Debug("Flushing buffered stdin data...")
				dc := b.activePeer.DataChannelTunnel()
				if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
					for _, data := range b.buffer {
						dc.Send(data)
					}
				}
				b.buffer = nil
			}
		}
	})

	b.manager.OnTunnelClose(func(peerID string) {
		b.mu.Lock()
		if b.activePeer != nil && b.activePeer.PeerID() == peerID {
			b.connected = false
			b.activePeer = nil
			logger.Debug("stdio bridge disconnected from peer: " + peerID)
		}
		b.mu.Unlock()
	})

	go b.readStdin()

	go b.keepAlive()

	<-b.done
	return nil
}

func (b *Bridge) readStdin() {
	buf := make([]byte, 32*1024)

	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			data := Wrap(StreamStdin, buf[:n])

			b.mu.Lock()
			if b.connected && b.activePeer != nil {
				dc := b.activePeer.DataChannelTunnel()
				if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
					if err := dc.Send(data); err != nil {
						logger.Debug("Failed to send stdin data: " + err.Error())
					}
				}
			} else {
				b.buffer = append(b.buffer, data)
				logger.Debug("Buffering stdin data (not connected yet)")
			}
			b.mu.Unlock()
		}

		if err != nil {
			if err != io.EOF {
				logger.Debug("stdin read error: " + err.Error())
			}
			break
		}
	}

	close(b.done)
}

func (b *Bridge) keepAlive() {
}

func (b *Bridge) Close() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}
