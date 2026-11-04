package service

import (
	"context"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func (r *Runtime) typingLoop(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			var keys []conversation.Key
			for key, count := range r.active {
				if count > 0 {
					keys = append(keys, key)
				}
			}
			r.mu.Unlock()
			for _, key := range keys {
				task, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = r.Engine.MM.Typing(task, key.Channel, key.Root)
				cancel()
			}
		}
	}
}
