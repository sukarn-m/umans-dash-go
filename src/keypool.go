package proxy

import (
	"time"
)

// ─── KeyPool (§3) ───────────────────────────────────────────────────────────

// ─── KeyPool (§3) ───────────────────────────────────────────────────────────

// NewKeyPool creates a new key pool from the given keys.
func NewKeyPool(keys []KeyConfig) *KeyPool {
	entries := make([]*keyEntry, len(keys))
	for i, k := range keys {
		entries[i] = &keyEntry{Key: k.Key, Name: k.Name, Healthy: true, CooldownMs: 30000}
	}
	return &KeyPool{entries: entries}
}

// SetConfig associates a Config with the pool so that Acquire can set the
// active API key per spec §3.2. Optional — if not called, Acquire skips
// the config update (useful for standalone testing).
func (p *KeyPool) SetConfig(cfg *Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = cfg
}

// Acquire returns a healthy key, round-robin. preferredIndex >= 0 tries that key first.
func (p *KeyPool) Acquire(preferredIndex int) (*KeySlot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil, false
	}
	now := time.Now()
	if preferredIndex >= 0 && preferredIndex < len(p.entries) {
		e := p.entries[preferredIndex]
		if e.Healthy || now.Sub(e.LastError) >= time.Duration(e.CooldownMs)*time.Millisecond {
			e.Healthy = true
			if p.config != nil {
				p.config.APIKey = e.Key
			}
			return &KeySlot{Key: e.Key, Name: e.Name, Index: preferredIndex}, true
		}
	}
	for i := 0; i < len(p.entries); i++ {
		idx := p.index % len(p.entries)
		p.index++
		e := p.entries[idx]
		if e.Healthy || now.Sub(e.LastError) >= time.Duration(e.CooldownMs)*time.Millisecond {
			e.Healthy = true
			if p.config != nil {
				p.config.APIKey = e.Key
			}
			return &KeySlot{Key: e.Key, Name: e.Name, Index: idx}, true
		}
	}
	return nil, false
}

// MarkUnhealthy marks a key as unhealthy with a cooldown based on status code.
func (p *KeyPool) MarkUnhealthy(index, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.entries) {
		return
	}
	e := p.entries[index]
	e.Healthy = false
	e.LastError = time.Now()
	if status >= 503 {
		e.CooldownMs = 60000
	} else if status >= 502 {
		e.CooldownMs = 30000
	} else {
		e.CooldownMs = 10000
	}
}

// MarkHealthy resets a key to healthy status.
func (p *KeyPool) MarkHealthy(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.entries) {
		return
	}
	p.entries[index].Healthy = true
	p.entries[index].LastError = time.Time{}
}

// HealthyCount returns the number of healthy or cooldown-expired keys.
func (p *KeyPool) HealthyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	count := 0
	for _, e := range p.entries {
		if e.Healthy || now.Sub(e.LastError) >= time.Duration(e.CooldownMs)*time.Millisecond {
			count++
		}
	}
	return count
}

// Total returns the total number of keys in the pool.
func (p *KeyPool) Total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// State returns the public state of all keys.
func (p *KeyPool) State() []KeyState {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	states := make([]KeyState, len(p.entries))
	for i, e := range p.entries {
		cool := int64(0)
		status := "none"
		if e.Key != "" {
			if !e.Healthy {
				cool = e.CooldownMs - now.Sub(e.LastError).Milliseconds()
				if cool <= 0 {
					cool = 0
					status = "active"
				} else {
					status = "cooldown"
				}
			} else {
				status = "active"
			}
		}
		states[i] = KeyState{
			Name:              e.Name,
			Status:            status,
			Healthy:           status == "active",
			RemainingCooldown: cool,
			Token:             MaskToken(e.Key),
		}
	}
	return states
}
