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
	logger.Init()
	defer logger.Sync()

	listenPort := flag.Int("listen", -1, "Local port to listen on")
	flag.IntVar(listenPort, "l", -1, "Local TCP listen (alias)")

	remotePort := flag.Int("remote-port", 25575, "Remote port to forward to")
	flag.IntVar(remotePort, "r", 25575, "Remote port to forward to (alias)")

	flag.Parse()

	logger.Debug("listenPort: " + strconv.Itoa(*listenPort))
	logger.Debug("remotePort: " + strconv.Itoa(*remotePort))

	url := flag.String("url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")
	roomID := flag.String("room", "p2p-tunnel-room", "Room ID for signaling")

	selfID := uuid.New().String()

	sig, err := signal.NewClient(*url, selfID, *roomID)
	if err != nil {
		logger.Error("Failed to connect signaling server:" + err.Error())
		return
	}

	peer := rtc.NewPeer(sig, selfID, *roomID)
	peer.OnChatMessage(func(msg string) {
		println(msg)
	})

	go tcp.ListenAndServe(peer, *listenPort, *remotePort)

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			peer.SendChat(scanner.Text())
		}
	}()

	select {}
}
