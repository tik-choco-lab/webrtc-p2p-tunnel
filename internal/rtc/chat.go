package rtc

func (p *Peer) SendChat(msg string) error {
	if p.dcChat == nil {
		return nil
	}
	return p.dcChat.SendText(msg)
}

func (p *Peer) OnChatMessage(fn func(string)) {
	p.chatCallbacks = append(p.chatCallbacks, fn)
}

func (p *Peer) OnChatClose(fn func()) {
	if p.dcChat != nil {
		p.dcChat.OnClose(func() {
			fn()
		})
	}
}
