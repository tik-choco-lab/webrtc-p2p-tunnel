package signal

import (
	"encoding/json"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
)

type Client struct {
	conn           *websocket.Conn
	onMsg          func(Message)
	onReconnect    func()
	selfID         string
	roomID         string
	wsURL          string
	mu             sync.Mutex
	reconnecting   bool
	maxRetries     int
	retryCount     int
	baseRetryDelay time.Duration
	maxRetryDelay  time.Duration
	closed         bool
	closeChan      chan struct{}
}

func NewClient(wsURL, selfID, roomID string) (*Client, error) {
	c := &Client{
		selfID:         selfID,
		roomID:         roomID,
		wsURL:          wsURL,
		maxRetries:     10,
		baseRetryDelay: 1 * time.Second,
		maxRetryDelay:  30 * time.Second,
		closeChan:      make(chan struct{}),
	}

	err := c.connect()
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) connect() error {
	u, _ := url.Parse(c.wsURL)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.retryCount = 0
	c.mu.Unlock()

	go c.readLoop()

	c.Send(Message{
		Type:     "Request",
		SenderId: c.selfID,
		RoomId:   c.roomID,
	})

	logger.Debug("WebSocket connected to " + c.wsURL)
	return nil
}

func (c *Client) OnMessage(fn func(Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMsg = fn
}

func (c *Client) OnReconnect(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onReconnect = fn
}

func (c *Client) Send(msg Message) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return websocket.ErrCloseSent
	}

	data, _ := json.Marshal(msg)
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) readLoop() {
	for {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			return
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			logger.Debug("WebSocket read error: " + err.Error())

			// クローズ済みの場合は再接続しない
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()

			if closed {
				return
			}

			// 再接続を試みる
			go c.reconnect()
			return
		}

		var m Message
		if err := json.Unmarshal(msg, &m); err == nil && c.onMsg != nil {
			c.onMsg(m)
		}
	}
}

func (c *Client) reconnect() {
	c.mu.Lock()
	if c.reconnecting {
		c.mu.Unlock()
		return
	}
	c.reconnecting = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.reconnecting = false
		c.mu.Unlock()
	}()

	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}

		if c.retryCount >= c.maxRetries {
			c.mu.Unlock()
			logger.Error("Max reconnection attempts reached. Giving up.")
			return
		}
		c.retryCount++
		currentRetry := c.retryCount
		c.mu.Unlock()

		delay := c.calculateBackoff(currentRetry)
		logger.Debug("Attempting to reconnect in " + delay.String() + " (attempt " +
			string(rune(currentRetry)) + "/" + string(rune(c.maxRetries)) + ")...")

		select {
		case <-time.After(delay):
		case <-c.closeChan:
			return
		}

		logger.Debug("Reconnecting to WebSocket...")
		err := c.connect()
		if err == nil {
			logger.Debug("Successfully reconnected to WebSocket")

			c.mu.Lock()
			callback := c.onReconnect
			c.mu.Unlock()

			if callback != nil {
				callback()
			}
			return
		}

		logger.Debug("Reconnection failed: " + err.Error())
	}
}

func (c *Client) calculateBackoff(retryCount int) time.Duration {
	delay := c.baseRetryDelay * time.Duration(1<<uint(retryCount-1))
	if delay > c.maxRetryDelay {
		delay = c.maxRetryDelay
	}
	return delay
}

func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	conn := c.conn
	c.mu.Unlock()

	close(c.closeChan)

	if conn != nil {
		conn.Close()
	}
}
