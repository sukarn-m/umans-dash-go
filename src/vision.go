package proxy

import (
	"context"
	"fmt"
	"sync"
)

// ─── Vision Handoff (§11) ──────────────────────────────────────────────────

func (p *Proxy) PerformVisionHandoff(ctx context.Context, payload map[string]interface{}, resolvedModel string, apiKey string, preCollectedParts ...[]ImagePart) int {
	if !p.NeedsVisionHandoff(resolvedModel) {
		return 0
	}

	var parts []ImagePart
	if len(preCollectedParts) > 0 && preCollectedParts[0] != nil {
		parts = preCollectedParts[0]
	} else {
		parts = CollectImageParts(payload)
	}

	if len(parts) == 0 {
		return 0
	}

	// Snapshot config fields under configMu.RLock to avoid data races with
	// HandleConfigPost (which writes under configMu.Lock).
	p.configMu.RLock()
	maxImages := p.Config.MaxImages
	handoffModel := p.Config.VisionHandoffModel
	p.configMu.RUnlock()

	// Cap image parts to avoid excessive handoff calls. Use MaxImages (the
	// same field used by LimitImagesInMessages). Fall back to 4 if unset.
	if maxImages <= 0 {
		maxImages = 4
	}
	if len(parts) > maxImages {
		parts = parts[:maxImages]
	}

	if handoffModel == "" {
		handoffModel = "umans-coder"
	}

	descriptions := make([]string, len(parts))
	var wg sync.WaitGroup
	for i := range parts {
		wg.Add(1)
		go func(idx int, dataURI string) {
			defer wg.Done()
			// 4.1: Use proxy-wide vision-handoff semaphore instead of local one.
			// Only acquire/release when p.visionHandoffSem != nil.
			if p.visionHandoffSem != nil {
				select {
				case p.visionHandoffSem <- struct{}{}:
				case <-ctx.Done():
					descriptions[idx] = "[Image analysis cancelled]"
					return
				}
				defer func() { <-p.visionHandoffSem }()
			}
			// 4.2: Check ctx.Err() before sending the handoff request.
			if ctx.Err() != nil {
				descriptions[idx] = "[Image analysis cancelled]"
				return
			}
			if p.Upstream == nil {
				descriptions[idx] = "[Image analysis unavailable: upstream not configured]"
				return
			}
			descriptions[idx] = p.analyzeImageViaHandoff(ctx, dataURI, handoffModel, apiKey)
		}(i, parts[i].DataURI)
	}
	wg.Wait()

	for i, ip := range parts {
		label := "[Image content — analyzed by vision module, shown as text because the active model cannot see images:]\n"
		if len(parts) > 1 {
			label = fmt.Sprintf("[Image %d content — analyzed by vision module, shown as text because the active model cannot see images:]\n", i+1)
		}
		content := ip.Container.([]interface{})
		content[ip.Index] = map[string]interface{}{
			"type": "text",
			"text": label + descriptions[i],
		}
	}

	return len(parts)
}

// dashboardTemplateData holds values injected into the HTML/JS templates at serve time.
