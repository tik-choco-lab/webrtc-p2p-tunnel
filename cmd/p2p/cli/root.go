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

const (
	ChatIDPrefixLen   = 8
	ProtocolPrefixLen = 6
)

var (
	cfgFile string
	verbose int

	signalingURL string
	roomID       string
	authStr      string
	allowList    []string
)

var RootCmd = &cobra.Command{
	Use:   "p2p [room-id]",
	Short: "WebRTC P2P Tunnel CLI",
	Long: `A P2P tunnel application using WebRTC.
Supports TCP/UDP forwarding and standard I/O bridging.

If no command is specified, p2p starts in Chat Mode.
If a room-id is provided, it joins that room; otherwise, it generates a random one.`,
	Args: cobra.MaximumNArgs(1),
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		logger.InitWithOptions(logger.Options{
			Verbosity: verbose,
			UseStderr: true,
		})
	},
	Run: func(cmd *cobra.Command, args []string) {
		url := viper.GetString("url")
		currentRoomID := viper.GetString("room")

		if len(args) > 0 {
			currentRoomID = args[0]
		}

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
			println("[" + peerID[:ChatIDPrefixLen] + "] " + msg)
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
		addr = f[ProtocolPrefixLen:]
	} else if strings.HasPrefix(f, "udp://") {
		proto = "udp"
		addr = f[ProtocolPrefixLen:]
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
	connectCmd.Flags().StringVar(&signalingURL, "url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")
	connectCmd.Flags().StringVar(&authStr, "auth", "", "Authentication (private key file path or invite code)")
	connectCmd.Flags().Lookup("auth").NoOptDefVal = "__DEFAULT__"

	viper.BindPFlag("url", connectCmd.Flags().Lookup("url"))
	viper.BindPFlag("auth", connectCmd.Flags().Lookup("auth"))

	RootCmd.AddCommand(connectCmd)
}

func initServe() {
	serveCmd.Flags().StringVar(&signalingURL, "url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")
	serveCmd.Flags().StringVar(&authStr, "auth", "", "Authorization (github:user, file path, key string, or 'invite')")
	serveCmd.Flags().Lookup("auth").NoOptDefVal = "__DEFAULT__"
	serveCmd.Flags().StringSliceVar(&allowList, "allow", []string{}, "Allow list (e.g. github:user, github:org/team)")

	viper.BindPFlag("url", serveCmd.Flags().Lookup("url"))
	viper.BindPFlag("auth", serveCmd.Flags().Lookup("auth"))
	viper.BindPFlag("allow", serveCmd.Flags().Lookup("allow"))

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
