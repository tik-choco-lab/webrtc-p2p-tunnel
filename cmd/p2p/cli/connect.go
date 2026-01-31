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
)

var connectCmd = &cobra.Command{
	Use:   "connect [room-id]",
	Short: "Connect to a tunnel room and bridge stdio",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		url := viper.GetString("url")
		currentRoomID := viper.GetString("room")

		if len(args) > 0 {
			currentRoomID = args[0]
		}

		if currentRoomID == "" {
			logger.Error("Room ID is required either via argument or --room flag")
			os.Exit(1)
		}

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

		if err := bridge.Run(); err != nil {
			logger.Error("stdio bridge error: " + err.Error())
		}

		manager.Close()
	},
}

func init() {
	RootCmd.AddCommand(connectCmd)
}
