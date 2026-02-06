package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/auth"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/proxy"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	signalclient "github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/tcp"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/udp"
)

const (
	RoomIDByteLen = 4
	ListenPort    = -1
)

var (
	serveForwards []string
)

func generateRoomID() string {
	b := make([]byte, RoomIDByteLen)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var serveCmd = &cobra.Command{
	Use:   "serve [room-id] [forward-target] [flags] -- [command...]",
	Short: "(Server side) Publish a command or port to a room",
	Example: `  # Auto-generate room and serve local port 80
  p2p serve :80

  # Execute command in specific room
  p2p serve my-room -- python server.py

  # Auth Examples:
  p2p serve my-room :80 --auth github:fog-zs
  p2p serve my-room :80 --auth github:org/tik-choco-lab
  p2p serve my-room :80 --auth invite`,
	Run: func(cmd *cobra.Command, args []string) {
		serveForwards = nil
		dashIdx := cmd.ArgsLenAtDash()
		var currentRoomID string
		var remoteCommand []string
		var dashArgs []string

		if dashIdx >= 0 {
			dashArgs = args[:dashIdx]
			remoteCommand = args[dashIdx:]
		} else {
			dashArgs = args
		}

		if len(dashArgs) > 0 {
			arg := dashArgs[0]
			if (strings.Contains(arg, ":") || strings.HasPrefix(arg, "tcp://") || strings.HasPrefix(arg, "udp://")) && len(dashArgs) == 1 {
				serveForwards = append(serveForwards, arg)
			} else {
				currentRoomID = arg
				if len(dashArgs) > 1 {
					serveForwards = append(serveForwards, dashArgs[1])
				}
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

		aStr := viper.GetString("auth")
		if aStr == "__DEFAULT__" {
			aStr = auth.GetDefaultAuthorizedKeysPath()
		}
		alists := viper.GetStringSlice("allow")

		if aStr != "" || len(alists) > 0 {
			ma := &auth.MultiAuthorizer{}
			if aStr != "" {
				authorizer, err := auth.CreateAuthorizer(aStr, currentRoomID)
				if err != nil {
					logger.Error("Failed to create authorizer: " + err.Error())
					return
				}
				ma.Add(authorizer)
			}
			for _, item := range alists {
				authorizer, err := auth.CreateAuthorizer(item, currentRoomID)
				if err != nil {
					logger.Warn("Failed to create authorizer for " + item + ": " + err.Error())
					continue
				}
				ma.Add(authorizer)
			}
			manager.SetAuthorizer(ma)
			logger.Info("Authorization enabled")
		}

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		for _, f := range serveForwards {
			proto, addr, _ := ParseForward(f)
			if proto == "tcp" {
				go tcp.ListenAndServe(manager, ListenPort, addr)
			} else {
				go udp.ListenAndServe(manager, ListenPort, addr)
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
			logger.Error("No command or forwards specified. Usage: p2p serve [room-id] [forward-target] ...")
		}

		manager.Close()
	},
}
