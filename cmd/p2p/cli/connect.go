package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/auth"
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
	Use:   "connect <room-id> [forward-target]",
	Short: "(Client side) Join a room and bridge to your stdio",
	Example: `  # Standard I/O bridge only
  p2p connect my-room

  # Standard I/O + Local listen (TCP or UDP)
  p2p connect my-room :8080
  p2p connect my-room tcp://:8080
  p2p connect my-room udp://:9000`,
	Args: cobra.RangeArgs(1, 2),
	Run: func(cmd *cobra.Command, args []string) {
		connectForwards = nil
		currentRoomID := args[0]
		if len(args) > 1 {
			connectForwards = append(connectForwards, args[1])
		}
		url := viper.GetString("url")

		selfID := uuid.New().String()
		sig, err := signalclient.NewClient(url, selfID, currentRoomID)
		if err != nil {
			logger.Error("Failed to connect signaling server:" + err.Error())
			return
		}

		manager := rtc.NewRTCManager(sig, selfID, currentRoomID, false)

		aStr := viper.GetString("auth")
		authenticator, err := auth.CreateAuthenticator(aStr)
		if err != nil {
			logger.Error("Failed to create authenticator: " + err.Error())
			return
		}
		if authenticator != nil {
			manager.SetAuthenticator(authenticator)
			logger.Info("Authentication enabled")
		}

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
