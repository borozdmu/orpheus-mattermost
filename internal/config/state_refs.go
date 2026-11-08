package config

import (
	"errors"
	"maps"
	"slices"
)

// ValidateStateRefs reserves old state partitions independently of agent roles.
// A migration must name every route explicitly, including denied routes. An
// alias never grants access and may not name another binding or another chat.
func ValidateStateRefs(workflows []Workflow) error {
	roots := make(map[string]bool, len(workflows))
	for _, w := range workflows {
		roots[w.ID] = true
	}
	owners := map[string]bool{}
	for _, w := range workflows {
		explicit := false
		for _, route := range w.ChatAccess {
			explicit = explicit || route.StateRef != ""
		}
		if !explicit {
			continue
		}
		for _, route := range w.ChatAccess {
			ref := route.StateRef
			if !namePattern.MatchString(ref) || owners[ref] || roots[ref] && ref != w.ID {
				return errors.New("state_ref must name a unique state partition owned by this binding")
			}
			owners[ref] = true
		}
	}
	return nil
}

// SameStateRefs prevents SIGHUP from moving existing state between chats.
// Changing state ownership requires a drained, independently checked restart.
func SameStateRefs(a, b []Workflow) bool {
	refs := func(workflows []Workflow) map[[3]string]string {
		result := map[[3]string]string{}
		for _, w := range workflows {
			for chat, route := range w.ChatAccess {
				if route.StateRef != "" {
					result[[3]string{w.ID, w.AgentID, chat}] = route.StateRef
				}
			}
		}
		return result
	}
	return maps.Equal(refs(a), refs(b))
}

// StateWorkflows resolves current authority and per-channel modes before old
// session namespaces are discovered. Denied or unknown routes are omitted.
func (c Config) StateWorkflows() []Workflow {
	if ValidateStateRefs(c.Workflows) != nil {
		return nil
	}
	var result []Workflow
	for _, w := range c.Workflows {
		if len(w.ChatAccess) == 0 {
			result = append(result, w)
			continue
		}
		for _, chat := range w.IncludeIDs {
			child, err := w.ForChat(chat)
			if err == nil {
				result = append(result, child)
			}
		}
	}
	return result
}

// StateWorkflow resolves an old namespace only for its configured channel.
func (c Config) StateWorkflow(id, chat string) *Workflow {
	for _, w := range c.StateWorkflows() {
		if w.ID == id && (len(w.IncludeIDs) == 0 || slices.Contains(w.IncludeIDs, chat)) && !slices.Contains(w.ExcludeIDs, chat) {
			return &w
		}
	}
	return nil
}

// CapacityKey keeps an agent's concurrency limit independent of state aliases.
func (w Workflow) CapacityKey() string {
	if w.AgentID != "" {
		return "agent/" + w.AgentID
	}
	return "workflow/" + w.ID
}
