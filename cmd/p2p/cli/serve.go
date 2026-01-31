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
	Use:   "serve -- [command...]",
	Short: "Serve a command or wait for connections",
	Long:  `Starts the tunnel in server mode. If a command is specified after '--', it will be executed when a peer connects, and its stdio will be bridged.`,
	Run: func(cmd *cobra.Command, args []string) {
		url := viper.GetString("url")
		currentRoomID := viper.GetString("room")

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

		if len(args) > 0 {
			logger.Debug("Running in proxy-cmd mode")
			executor := proxy.NewExecutor(manager, args)

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
			logger.Error("No command or forwards specified. Usage: tunnel serve -- <command> OR tunnel serve -F <forward>")
		}

		manager.Close()
	},
}

func init() {
	serveCmd.Flags().StringSliceVarP(&serveForwards, "forward", "F", []string{}, "Forward target (e.g. tcp://127.0.0.1:80 or udp://127.0.0.1:9000)")
	RootCmd.AddCommand(serveCmd)
}
