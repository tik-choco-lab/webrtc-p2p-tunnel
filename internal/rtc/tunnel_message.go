package rtc

type TunnelMessage struct {
	Type    string `json:"type"` // "connect"/"data"/"close"
	ConnID  string `json:"conn_id"`
	Payload []byte `json:"payload,omitempty"`
}
