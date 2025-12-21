package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/google/uuid"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/proxy"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/rtc"
	signalclient "github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/stdio"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/tcp"
)

func generateRoomID() string {
	b := make([]byte, 4) // 8文字の16進数
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	// TCP tunnel flags
	listenPort := flag.Int("listen", -1, "Local port to listen on")
	flag.IntVar(listenPort, "l", -1, "Local TCP listen (alias)")

	remotePort := flag.Int("remote-port", -1, "Remote port to forward to")
	flag.IntVar(remotePort, "r", -1, "Remote port to forward to (alias)")

	isServer := flag.Bool("server", false, "Run as server mode (data receiver)")
	flag.BoolVar(isServer, "s", false, "Run as server mode (alias)")

	// MCP/stdio flags
	stdioMode := flag.Bool("stdio", false, "Run in stdio bridge mode (for MCP host)")
	proxyCmd := flag.String("proxy-cmd", "", "Command to execute and proxy stdio (for remote MCP server)")

	// Common flags
	debugMode := flag.Bool("debug", false, "Enable debug logging")
	flag.BoolVar(debugMode, "d", false, "Enable debug logging (alias)")

	url := flag.String("url", "wss://rtc.tik-choco.com/signaling", "Signaling server URL")
	roomID := flag.String("room", "", "Room ID for signaling (auto-generated if not specified)")

	flag.Parse()

	// stdioモードまたはproxy-cmdモードではstderrにログを出力
	useStderr := *stdioMode || *proxyCmd != ""
	logger.InitWithOptions(logger.Options{
		Debug:     *debugMode,
		UseStderr: useStderr,
	})
	defer logger.Sync()

	// room IDが未指定の場合、ランダム生成
	actualRoomID := *roomID
	if actualRoomID == "" {
		actualRoomID = generateRoomID()
		// room IDを出力（stdioモードではstderrに、それ以外はstdoutに）
		roomMsg := "Room ID: " + actualRoomID
		if useStderr {
			os.Stderr.WriteString(roomMsg + "\n")
		} else {
			println(roomMsg)
		}
	}

	selfID := uuid.New().String()

	sig, err := signalclient.NewClient(*url, selfID, actualRoomID)
	if err != nil {
		logger.Error("Failed to connect signaling server:" + err.Error())
		return
	}

	manager := rtc.NewRTCManager(sig, selfID, actualRoomID, *isServer)

	// シグナルハンドリング（クリーンアップ用）
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// モード判定
	if *stdioMode {
		// stdioブリッジモード
		logger.Debug("Running in stdio bridge mode")
		bridge := stdio.NewBridge(manager)

		go func() {
			<-sigChan
			bridge.Close()
		}()

		if err := bridge.Run(); err != nil {
			logger.Error("stdio bridge error: " + err.Error())
		}

	} else if *proxyCmd != "" {
		// proxy-cmdモード
		logger.Debug("Running in proxy-cmd mode: " + *proxyCmd)
		executor := proxy.NewExecutor(manager, *proxyCmd)

		go func() {
			<-sigChan
			executor.Close()
		}()

		if err := executor.Run(); err != nil {
			logger.Error("proxy executor error: " + err.Error())
		}

	} else {
		// 従来のTCPトンネル/チャットモード
		chatOnlyMode := *listenPort == -1 && *remotePort == -1

		if chatOnlyMode {
			println("=== Chat Mode ===")
			println("Type a message and press Enter to send.")
		} else {
			logger.Debug("listenPort: " + strconv.Itoa(*listenPort))
			logger.Debug("remotePort: " + strconv.Itoa(*remotePort))
			logger.Debug("isServer: " + strconv.FormatBool(*isServer))
		}

		manager.OnChatMessage(func(peerID string, msg string) {
			println("[" + peerID[:8] + "] " + msg)
		})

		if !chatOnlyMode {
			manager.OnTunnelOpen(func(peerID string) {
				logger.Debug("Tunnel opened with peer: " + peerID)
			})

			go tcp.ListenAndServe(manager, *listenPort, *remotePort)
		}

		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				manager.SendChatToAll(scanner.Text())
			}
		}()

		<-sigChan
	}

	manager.Close()
}
