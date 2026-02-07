package stdio

import (
	"io"
	"os"
	"sync"

	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
)

const (
	StdinBufferSize = 32 * 1024
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
	b.manager.OnStdioMessage(func(peerID string, data []byte) {
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

	b.manager.OnStdioOpen(func(peerID string) {
		b.handleStdioOpen(peerID)
	})

	b.manager.OnStdioClose(func(peerID string) {
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

func (b *Bridge) handleStdioOpen(peerID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.connected = true
	b.activePeer = b.manager.GetPeer(peerID)
	logger.Debug("stdio bridge connected to peer: " + peerID)

	b.flushBuffer(b.activePeer)
}

func (b *Bridge) flushBuffer(peer *rtc.RemotePeer) {
	if len(b.buffer) == 0 || peer == nil {
		return
	}

	logger.Debug("Flushing buffered stdin data...")
	for _, data := range b.buffer {
		peer.SendStdio(data)
	}
	b.buffer = nil
}

func (b *Bridge) readStdin() {
	buf := make([]byte, StdinBufferSize)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			b.processInput(buf[:n])
		}
		if err != nil {
			b.handleReadError(err)
			break
		}
	}
	close(b.done)
}

func (b *Bridge) processInput(payload []byte) {
	data := Wrap(StreamStdin, payload)
	b.mu.Lock()
	defer b.mu.Unlock()

	b.sendOrBuffer(data)
}

func (b *Bridge) sendOrBuffer(data []byte) {
	if !b.connected {
		b.bufferInput(data)
		return
	}
	b.manager.BroadcastStdio(data)
}

func (b *Bridge) bufferInput(data []byte) {
	b.buffer = append(b.buffer, data)
	logger.Debug("Buffering stdin data (not connected yet)")
}

func (b *Bridge) handleReadError(err error) {
	if err != io.EOF {
		logger.Debug("stdin read error: " + err.Error())
	}
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
