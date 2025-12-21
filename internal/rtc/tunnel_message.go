package rtc

type TunnelMessage struct {
	Type    string `json:"type"`
	ConnID  string `json:"conn_id"`
	Payload []byte `json:"payload,omitempty"`
}
