package rtc

import (
	"sync"
	"time"

	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/signal"
)

type signalingRouter struct {
	selfID   string
	cache    map[string]time.Time
	mu       sync.Mutex
	onRelay  func(signal.Message)
	onTarget func(signal.Message)
	quit     chan struct{}
}

func newSignalingRouter(selfID string, onRelay, onTarget func(signal.Message)) *signalingRouter {
	r := &signalingRouter{
		selfID:   selfID,
		cache:    make(map[string]time.Time),
		onRelay:  onRelay,
		onTarget: onTarget,
		quit:     make(chan struct{}),
	}
	go r.cleanupLoop()
	return r
}

func (r *signalingRouter) receive(msg signal.Message) {
	if msg.MsgID != "" {
		r.mu.Lock()
		if _, exists := r.cache[msg.MsgID]; exists {
			r.mu.Unlock()
			return
		}
		r.cache[msg.MsgID] = time.Now()
		r.mu.Unlock()
	}

	if msg.ReceiverId != "" && msg.ReceiverId != r.selfID {
		r.onRelay(msg)
		return
	}

	if msg.ReceiverId == "" {
		r.onRelay(msg)
	}
	r.onTarget(msg)
}

func (r *signalingRouter) cleanupLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			now := time.Now()
			for id, t := range r.cache {
				if now.Sub(t) > 5*time.Minute {
					delete(r.cache, id)
				}
			}
			r.mu.Unlock()
		case <-r.quit:
			return
		}
	}
}

func (r *signalingRouter) stop() {
	select {
	case <-r.quit:
		return
	default:
		close(r.quit)
	}
}
