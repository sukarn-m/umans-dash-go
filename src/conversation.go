package proxy

import (
	"container/list"
)

// ─── Conversation Tracking (§14) ─────────────────────────────────────────────

// ─── Conversation Tracking (§14) ─────────────────────────────────────────────

// ensureConversationMap lazily initializes conversation tracking state.
// Must be called under convMu.
func (p *Proxy) ensureConversationMap() {
	if p.conversationMap == nil {
		p.conversationMap = make(map[string]*ConversationSession)
		p.convLRU = list.New()
		p.convLRUIndex = make(map[string]*list.Element)
	}
}

// TouchConversation returns the ConversationSession for the given fingerprint,
// moving it to the most-recently-used position in the LRU. Returns nil if the
// fingerprint is empty or not found in the map. (Spec §14.3)
func (p *Proxy) TouchConversation(fingerprint string) *ConversationSession {
	if fingerprint == "" {
		return nil
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	p.ensureConversationMap()
	elem, ok := p.convLRUIndex[fingerprint]
	if !ok {
		return nil
	}
	p.convLRU.MoveToFront(elem)
	return p.conversationMap[fingerprint]
}

// TrackConversationSession stores or updates a conversation session in the map.
// If alreadyTouched is true, the fingerprint is assumed to already be at the
// most-recently-used position, so only the session value is set. Otherwise the
// entry is moved to most-recently-used. If the map is at capacity (ConvMapMax)
// and the fingerprint is new, entries are evicted from the LRU tail until the
// size reaches ConvMapEvictTarget. (Spec §14.4)
func (p *Proxy) TrackConversationSession(fingerprint string, session *ConversationSession, alreadyTouched bool) {
	if fingerprint == "" {
		return
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	p.ensureConversationMap()

	_, exists := p.conversationMap[fingerprint]

	// Eviction: only if at capacity AND fingerprint is new (Spec §14.4 lines 685-686)
	if !exists && len(p.conversationMap) >= ConvMapMax {
		for len(p.conversationMap) > ConvMapEvictTarget {
			back := p.convLRU.Back()
			if back == nil {
				break
			}
			oldFp := back.Value.(string)
			p.convLRU.Remove(back)
			delete(p.conversationMap, oldFp)
			delete(p.convLRUIndex, oldFp)
		}
	}

	// Set the session value
	p.conversationMap[fingerprint] = session

	// Update LRU position (Spec §14.4 lines 687-688)
	// Line 687: If alreadyTouched → just set the fingerprint (LRU already
	// updated by a prior TouchConversation call, so skip the move).
	// Line 688: Otherwise → move to most-recently-used.
	if !alreadyTouched {
		if elem, ok := p.convLRUIndex[fingerprint]; ok {
			p.convLRU.MoveToFront(elem)
		} else {
			elem := p.convLRU.PushFront(fingerprint)
			p.convLRUIndex[fingerprint] = elem
		}
	}
}
