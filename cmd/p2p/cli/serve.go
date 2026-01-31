package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/proxy"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	signalclient "github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/tcp"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/udp"
)

var (
	serveForwards []string
)

func generateRoomID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var serveCmd = &cobra.Command{
	Use:   "serve [room-id] [flags] -- [command...]",
	Short: "(Server side) Publish a command or port to a room",
	Run: func(cmd *cobra.Command, args []string) {
		dashIdx := cmd.ArgsLenAtDash()
		var currentRoomID string
		var remoteCommand []string

		if dashIdx >= 0 {
			if dashIdx > 0 {
				currentRoomID = args[0]
			}
			remoteCommand = args[dashIdx:]
		} else {
			if len(args) > 0 {
				currentRoomID = args[0]
			}
		}

		if currentRoomID == "" {
			currentRoomID = viper.GetString("room")
		}

		url := viper.GetString("url")
		if currentRoomID == "" {
			currentRoomID = generateRoomID()
			fmt.Fprintf(os.Stderr, "Room ID: %s\n", currentRoomID)
		}

		selfID := uuid.New().String()
		sig, err := signalclient.NewClient(url, selfID, currentRoomID)
		if err != nil {
			logger.Error("Failed to connect signaling server:" + err.Error())
			return
		}

		manager := rtc.NewRTCManager(sig, selfID, currentRoomID, true)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		for _, f := range serveForwards {
			proto, addr, _ := ParseForward(f)
			if proto == "tcp" {
				go tcp.ListenAndServe(manager, -1, addr)
			} else {
				go udp.ListenAndServe(manager, -1, addr)
			}
		}

		if len(remoteCommand) > 0 {
			logger.Debug("Running in proxy-cmd mode")
			executor := proxy.NewExecutor(manager, remoteCommand)

			go func() {
				<-sigChan
				executor.Close()
				os.Exit(0)
			}()

			if err := executor.Run(); err != nil {
				logger.Error("proxy executor error: " + err.Error())
			}
		} else if len(serveForwards) > 0 {
			logger.Debug("Running in port-forwarding mode")
			<-sigChan
		} else {
			logger.Error("No command or forwards specified. Usage: p2p serve [room-id] -- <command> OR p2p serve [room-id] -F <forward>")
		}

		manager.Close()
	},
}
