package proxy

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
)

type Executor struct {
	manager    *rtc.RTCManager
	command    string
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	mu         sync.Mutex
	activePeer *rtc.RemotePeer
	done       chan struct{}
}

func NewExecutor(manager *rtc.RTCManager, command string) *Executor {
	return &Executor{
		manager: manager,
		command: command,
		done:    make(chan struct{}),
	}
}

func (e *Executor) Run() error {
	e.manager.OnTunnelMessage(func(peerID string, data []byte) {
		e.mu.Lock()
		defer e.mu.Unlock()

		if e.activePeer == nil || e.activePeer.PeerID() != peerID {
			e.activePeer = e.manager.GetPeer(peerID)
		}

		if e.stdin != nil {
			if _, err := e.stdin.Write(data); err != nil {
				logger.Debug("Failed to write to child stdin: " + err.Error())
			}
		}
	})

	e.manager.OnTunnelOpen(func(peerID string) {
		e.mu.Lock()
		defer e.mu.Unlock()

		if e.cmd != nil {
			return
		}

		e.activePeer = e.manager.GetPeer(peerID)
		logger.Debug("Starting proxy command for peer: " + peerID)

		if err := e.startCommand(); err != nil {
			logger.Error("Failed to start proxy command: " + err.Error())
		}
	})

	e.manager.OnTunnelClose(func(peerID string) {
		e.mu.Lock()
		if e.activePeer != nil && e.activePeer.PeerID() == peerID {
			logger.Debug("Tunnel closed, stopping proxy command")
			e.stopCommand()
			e.activePeer = nil
		}
		e.mu.Unlock()
	})

	go e.keepAlive()

	<-e.done
	return nil
}

func (e *Executor) startCommand() error {
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", e.command)
	} else {
		cmd = exec.Command("sh", "-c", e.command)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	e.stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	e.cmd = cmd
	logger.Debug("Proxy command started: " + e.command)

	go e.forwardStdout(stdout)

	go func() {
		err := cmd.Wait()
		e.mu.Lock()
		e.cmd = nil
		e.stdin = nil
		e.mu.Unlock()

		if err != nil {
			logger.Debug("Proxy command exited with error: " + err.Error())
		} else {
			logger.Debug("Proxy command exited normally")
		}
	}()

	return nil
}

func (e *Executor) forwardStdout(stdout io.ReadCloser) {
	buf := make([]byte, 64*1024)

	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			e.mu.Lock()
			peer := e.activePeer
			e.mu.Unlock()

			if peer != nil {
				dc := peer.DataChannelTunnel()
				if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
					if err := dc.Send(data); err != nil {
						logger.Debug("Failed to send stdout data: " + err.Error())
					}
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				logger.Debug("stdout read error: " + err.Error())
			}
			break
		}
	}
}

func (e *Executor) stopCommand() {
	if e.cmd == nil || e.cmd.Process == nil {
		return
	}

	if e.stdin != nil {
		e.stdin.Close()
		e.stdin = nil
	}

	if runtime.GOOS == "windows" {
		e.cmd.Process.Kill()
	} else {
		e.cmd.Process.Signal(os.Interrupt)

		done := make(chan struct{})
		go func() {
			e.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			e.cmd.Process.Kill()
		}
	}

	e.cmd = nil
}

func (e *Executor) keepAlive() {
	<-e.done
}

func (e *Executor) Close() {
	e.mu.Lock()
	e.stopCommand()
	e.mu.Unlock()

	select {
	case <-e.done:
	default:
		close(e.done)
	}
}
