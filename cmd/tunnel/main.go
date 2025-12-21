package main

import (
	"bufio"
	"flag"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/tcp"
)

func main() {
	listenPort := flag.Int("listen", -1, "Local port to listen on")
	flag.IntVar(listenPort, "l", -1, "Local TCP listen (alias)")

	remotePort := flag.Int("remote-port", -1, "Remote port to forward to")
	flag.IntVar(remotePort, "r", -1, "Remote port to forward to (alias)")

	isServer := flag.Bool("server", false, "Run as server mode (data receiver)")
	flag.BoolVar(isServer, "s", false, "Run as server mode (alias)")

	debugMode := flag.Bool("debug", false, "Enable debug logging")
	flag.BoolVar(debugMode, "d", false, "Enable debug logging (alias)")

	url := flag.String("url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")
	roomID := flag.String("room", "p2p-tunnel-room", "Room ID for signaling")

	flag.Parse()

	logger.InitWithDebug(*debugMode)
	defer logger.Sync()

	chatOnlyMode := *listenPort == -1 && *remotePort == -1

	if chatOnlyMode {
		println("=== Chat Mode ===")
		println("Type a message and press Enter to send.")
	} else {
		logger.Debug("listenPort: " + strconv.Itoa(*listenPort))
		logger.Debug("remotePort: " + strconv.Itoa(*remotePort))
		logger.Debug("isServer: " + strconv.FormatBool(*isServer))
	}

	selfID := uuid.New().String()

	sig, err := signal.NewClient(*url, selfID, *roomID)
	if err != nil {
		logger.Error("Failed to connect signaling server:" + err.Error())
		return
	}

	manager := rtc.NewRTCManager(sig, selfID, *roomID, *isServer)

	manager.OnChatMessage(func(peerID string, msg string) {
		println("[" + peerID[:8] + "] " + msg)
	})

	if !chatOnlyMode {
		manager.OnTunnelOpen(func(peerID string) {
			logger.Debug("Tunnel opened with peer: " + peerID)
		})

		go tcp.ListenAndServe(manager, *listenPort, *remotePort)
	}

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			manager.SendChatToAll(scanner.Text())
		}
	}()

	select {}
}
