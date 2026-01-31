package signal

type Message struct {
	Type       string `json:"Type"`
	Data       string `json:"Data,omitempty"`
	SenderId   string `json:"SenderId"`
	ReceiverId string `json:"ReceiverId,omitempty"`
	RoomId     string `json:"RoomId,omitempty"`
	Role       string `json:"Role,omitempty"`
	MsgID      string `json:"MsgID,omitempty"`
	Hops       int    `json:"Hops,omitempty"`
}
