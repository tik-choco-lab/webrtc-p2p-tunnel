package cli

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	signalclient "github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
)

var (
	cfgFile string
	verbose int

	signalingURL string
	roomID       string
)

var RootCmd = &cobra.Command{
	Use:   "p2p",
	Short: "WebRTC P2P Tunnel CLI",
	Long: `A P2P tunnel application using WebRTC.
Supports TCP/UDP forwarding and standard I/O bridging.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		logger.InitWithOptions(logger.Options{
			Verbosity: verbose,
			UseStderr: true,
		})
	},
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

		manager := rtc.NewRTCManager(sig, selfID, currentRoomID, false)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		println("=== Chat Mode ===")
		println("Type a message and press Enter to send.")

		manager.OnChatMessage(func(peerID string, msg string) {
			println("[" + peerID[:8] + "] " + msg)
		})

		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				manager.SendChatToAll(scanner.Text())
			}
		}()

		<-sigChan
		manager.Close()
	},
}

func ParseForward(f string) (string, string, int) {
	proto := "tcp"
	addr := f
	if strings.HasPrefix(f, "tcp://") {
		proto = "tcp"
		addr = f[6:]
	} else if strings.HasPrefix(f, "udp://") {
		proto = "udp"
		addr = f[6:]
	}

	port := -1
	if p, err := strconv.Atoi(addr); err == nil {
		port = p
	} else {
		_, portStr, err := net.SplitHostPort(addr)
		if err == nil {
			if p, err := strconv.Atoi(portStr); err == nil {
				port = p
			}
		}
	}
	return proto, addr, port
}

func Execute() {
	if err := RootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	RootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file path")
	RootCmd.PersistentFlags().CountVarP(&verbose, "verbose", "v", "-v for info, -vv for debug")

	RootCmd.CompletionOptions.HiddenDefaultCmd = true
	RootCmd.SetHelpCommand(&cobra.Command{Use: "nohelp", Hidden: true})

	initConnect()
	initServe()
}

func initConnect() {
	connectCmd.Flags().StringSliceVarP(&connectForwards, "forward", "F", []string{}, "Local listen address to forward (e.g. tcp://:8080 or udp://:9000)")

	connectCmd.Flags().StringVar(&signalingURL, "url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")

	viper.BindPFlag("url", connectCmd.Flags().Lookup("url"))

	RootCmd.AddCommand(connectCmd)
}

func initServe() {
	serveCmd.Flags().StringSliceVarP(&serveForwards, "forward", "F", []string{}, "Remote target address to dial (e.g. tcp://127.0.0.1:80 or udp://127.0.0.1:9000)")

	serveCmd.Flags().StringVar(&signalingURL, "url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")

	viper.BindPFlag("url", serveCmd.Flags().Lookup("url"))

	RootCmd.AddCommand(serveCmd)
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err == nil {
			viper.AddConfigPath(home)
			viper.SetConfigName(".tunnel")
		}
	}

	viper.SetEnvPrefix("TUNNEL")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil && verbose > 0 {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}
