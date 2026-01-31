package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	signalclient "github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/stdio"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/tcp"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/udp"
)

var (
	connectForwards []string
)

var connectCmd = &cobra.Command{
	Use:   "connect <room-id>",
	Short: "(Client side) Join a room and bridge to your stdio",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		currentRoomID := args[0]
		url := viper.GetString("url")

		selfID := uuid.New().String()
		sig, err := signalclient.NewClient(url, selfID, currentRoomID)
		if err != nil {
			logger.Error("Failed to connect signaling server:" + err.Error())
			return
		}

		manager := rtc.NewRTCManager(sig, selfID, currentRoomID, false)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		logger.Debug("Running in stdio bridge mode")
		bridge := stdio.NewBridge(manager)

		go func() {
			<-sigChan
			bridge.Close()
			os.Exit(0)
		}()

		for _, f := range connectForwards {
			proto, _, port := ParseForward(f)
			if proto == "tcp" {
				go tcp.ListenAndServe(manager, port, "")
			} else {
				go udp.ListenAndServe(manager, port, "")
			}
		}

		if err := bridge.Run(); err != nil {
			logger.Error("stdio bridge error: " + err.Error())
		}

		manager.Close()
	},
}
