package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	signalclient "github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/tcp"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/udp"
)

var (
	cfgFile string
	verbose bool
	debug   bool

	signalingURL string
	roomID       string

	listenPort int
	remoteAddr string
	isServer   bool

	udpListenPort int
	udpRemoteAddr string
)

var RootCmd = &cobra.Command{
	Use:   "p2p",
	Short: "WebRTC P2P Tunnel CLI",
	Long: `A P2P tunnel application using WebRTC.
Supports TCP forwarding, standard I/O bridging, and remote command execution.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		logger.InitWithOptions(logger.Options{
			Debug:     debug || verbose,
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

		manager := rtc.NewRTCManager(sig, selfID, currentRoomID, isServer)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		chatOnlyMode := listenPort == -1 && remoteAddr == "" && udpListenPort == -1 && udpRemoteAddr == ""

		if chatOnlyMode {
			println("=== Chat Mode ===")
			println("Type a message and press Enter to send.")
		} else {
			if listenPort != -1 || remoteAddr != "" {
				logger.Debug("TCP listenPort: " + strconv.Itoa(listenPort))
				logger.Debug("TCP remoteAddr: " + remoteAddr)
			}
			if udpListenPort != -1 || udpRemoteAddr != "" {
				logger.Debug("UDP listenPort: " + strconv.Itoa(udpListenPort))
				logger.Debug("UDP remoteAddr: " + udpRemoteAddr)
			}
			logger.Debug("isServer: " + strconv.FormatBool(isServer))
		}

		manager.OnChatMessage(func(peerID string, msg string) {
			println("[" + peerID[:8] + "] " + msg)
		})

		if !chatOnlyMode {
			manager.OnTunnelOpen(func(peerID string) {
				logger.Debug("Tunnel opened with peer: " + peerID)
			})

			if listenPort != -1 || remoteAddr != "" {
				go tcp.ListenAndServe(manager, listenPort, remoteAddr)
			}
			if udpListenPort != -1 || udpRemoteAddr != "" {
				go udp.ListenAndServe(manager, udpListenPort, udpRemoteAddr)
			}
		}

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

func Execute() {
	if err := RootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	RootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.tunnel.yaml)")

	RootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	RootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "debug output")

	RootCmd.PersistentFlags().StringVar(&signalingURL, "url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")
	RootCmd.PersistentFlags().StringVar(&roomID, "room", "", "Room ID (auto-generated if empty)")

	RootCmd.Flags().IntVarP(&listenPort, "listen", "l", -1, "Local TCP port to listen on")
	RootCmd.Flags().StringVarP(&remoteAddr, "remote", "r", "", "Remote TCP address to forward to")
	RootCmd.Flags().BoolVarP(&isServer, "server", "s", false, "Run as server mode (data receiver)")

	RootCmd.Flags().IntVar(&udpListenPort, "udp-listen", -1, "Local UDP port to listen on")
	RootCmd.Flags().StringVar(&udpRemoteAddr, "udp-remote", "", "Remote UDP address to forward to")

	viper.BindPFlag("url", RootCmd.PersistentFlags().Lookup("url"))
	viper.BindPFlag("room", RootCmd.PersistentFlags().Lookup("room"))
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

	if err := viper.ReadInConfig(); err == nil && (debug || verbose) {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}
