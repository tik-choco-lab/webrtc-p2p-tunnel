package signal

import (
	"encoding/json"
	"net/url"

	"github.com/gorilla/websocket"
	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
)

type Client struct {
	conn   *websocket.Conn
	onMsg  func(Message)
	selfID string
	roomID string
}

func NewClient(wsURL, selfID, roomID string) (*Client, error) {
	u, _ := url.Parse(wsURL)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, selfID: selfID, roomID: roomID}
	go c.readLoop()
	c.Send(Message{
		Type:     "Request",
		SenderId: selfID,
		RoomId:   roomID,
	})
	return c, nil
}

func (c *Client) OnMessage(fn func(Message)) { c.onMsg = fn }

func (c *Client) Send(msg Message) error {
	data, _ := json.Marshal(msg)
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) readLoop() {
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			logger.Debug("read:" + err.Error())
			return
		}
		var m Message
		if err := json.Unmarshal(msg, &m); err == nil && c.onMsg != nil {
			c.onMsg(m)
		}
	}
}

func (c *Client) Close() {
	c.conn.Close()
}
