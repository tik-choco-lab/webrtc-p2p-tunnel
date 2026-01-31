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
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/stdio"
)

type Executor struct {
	manager    *rtc.RTCManager
	command    []string
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	mu         sync.Mutex
	activePeer *rtc.RemotePeer
	done       chan struct{}
}

func NewExecutor(manager *rtc.RTCManager, command []string) *Executor {
	return &Executor{
		manager: manager,
		command: command,
		done:    make(chan struct{}),
	}
}

func (e *Executor) Run() error {
	e.manager.OnStdioMessage(func(peerID string, data []byte) {
		e.mu.Lock()
		defer e.mu.Unlock()

		if e.activePeer == nil || e.activePeer.PeerID() != peerID {
			e.activePeer = e.manager.GetPeer(peerID)
		}

		if e.stdin != nil {
			streamType, payload := stdio.Unwrap(data)
			if streamType == stdio.StreamStdin {
				e.stdin.Write(payload)
			}
		}
	})

	e.manager.OnStdioOpen(func(peerID string) {
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

	e.manager.OnStdioClose(func(peerID string) {
		e.mu.Lock()
		if e.activePeer != nil && e.activePeer.PeerID() == peerID {
			logger.Debug("Stdio closed, stopping proxy command")
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
	if len(e.command) == 0 {
		return nil
	}

	cmd := exec.Command(e.command[0], e.command[1:]...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	e.stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	e.cmd = cmd
	logger.Debug("Proxy command started")

	go e.forwardStream(stdout, stdio.StreamStdout)
	go e.forwardStream(stderr, stdio.StreamStderr)

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

func (e *Executor) forwardStream(reader io.ReadCloser, streamType stdio.StreamType) {
	buf := make([]byte, 32*1024)

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := stdio.Wrap(streamType, buf[:n])

			e.mu.Lock()
			peer := e.activePeer
			e.mu.Unlock()

			if peer != nil {
				dc := peer.DataChannelStdio()
				if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
					dc.Send(data)
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				logger.Debug("stream read error: " + err.Error())
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
