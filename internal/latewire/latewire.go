// Package latewire catches the one way the one-call Instrument helpers fail
// silently: something attached to the loop after Instrument ran.
package latewire

import (
	"context"
	"log"
	"slices"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Watch snapshots what Instrument covered and, on the loop's first event —
// once, so it costs nothing afterwards — logs a warning naming anything
// attached later: tools, and with hooks, the HITL callback and permission
// policy. Compared by name, so wrapping a tool in more middleware is not a
// false alarm.
func Watch(loop *agent.AgentLoop, component string, hooks bool) {
	known := names(loop.Tools)
	hadHITL, hadPerms := loop.ConfirmHITL != nil, loop.Permissions != nil
	var once sync.Once
	loop.OnEvent(func(context.Context, string, agent.StreamEvent) {
		once.Do(func() {
			var late []string
			for _, n := range names(loop.Tools) {
				if !slices.Contains(known, n) {
					late = append(late, "tool "+n)
				}
			}
			if hooks && !hadHITL && loop.ConfirmHITL != nil {
				late = append(late, "ConfirmHITL")
			}
			if hooks && !hadPerms && loop.Permissions != nil {
				late = append(late, "Permissions")
			}
			if len(late) > 0 {
				log.Printf("[gopheragent] %s: %v attached after Instrument and not covered — call Instrument last", component, late)
			}
		})
	})
}

func names(r *tools.Registry) []string {
	if r == nil {
		return nil
	}
	all := r.All()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Descriptor().Name)
	}
	return out
}
