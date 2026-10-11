package config

import (
	"errors"
	"slices"

	"github.com/google/uuid"
)

// AdmissionCheckpoint imports only consumed IDs. It never grants data access or
// carries old model context. The owner operator supplies it after draining both
// admission and delivery; configuration validation is not a handover receipt.
type AdmissionCheckpoint struct {
	Schema          string   `yaml:"schema" json:"schema"`
	ChannelID       string   `yaml:"channel_id" json:"channel_id"`
	BotID           string   `yaml:"bot_id" json:"bot_id"`
	Epoch           string   `yaml:"epoch" json:"epoch"`
	FloorMS         int64    `yaml:"floor_ms" json:"floor_ms"`
	ReplayFromMS    int64    `yaml:"replay_from_ms" json:"replay_from_ms"`
	ConsumedPostIDs []string `yaml:"consumed_post_ids" json:"consumed_post_ids"`
}

func (w Workflow) validateCheckpoint() error {
	c := w.AdmissionCheckpoint
	if c == nil {
		return nil
	}
	epoch, err := uuid.Parse(c.Epoch)
	if err != nil || epoch == uuid.Nil || epoch.String() != c.Epoch || c.Schema != "mattermost-admission-checkpoint.v1" || !ValidID(c.ChannelID) || !ValidID(c.BotID) || c.FloorMS <= 0 || c.ReplayFromMS < c.FloorMS || c.ReplayFromMS > 253402300799999 || len(c.ConsumedPostIDs) > 10000 {
		return errors.New("invalid admission checkpoint")
	}
	// One checkpoint governs one explicit channel. No linked thread can bypass
	// the imported history boundary. The epoch participates in EffectiveRevision.
	if !slices.Equal(w.IncludeIDs, []string{c.ChannelID}) || len(w.ExcludeIDs) != 0 || w.DirectMessages || w.GroupMessages || *w.Links.Enabled || w.Since.UnixMilli() != c.ReplayFromMS || w.Since.Nanosecond()%1000000 != 0 {
		return errors.New("admission checkpoint requires one channel, exact replay time and disabled links")
	}
	if w.Access != nil && w.Access.BotID != c.BotID {
		return errors.New("admission checkpoint bot mismatch")
	}
	for i, id := range c.ConsumedPostIDs {
		if !ValidID(id) || i > 0 && id <= c.ConsumedPostIDs[i-1] {
			return errors.New("admission checkpoint IDs must be valid, unique and sorted")
		}
	}
	return nil
}

// Allows uses an inclusive replay time and an ID deny set, never an ID cursor.
// An edited consumed post stays consumed. A new lower-sorted ID in the same
// millisecond is still eligible, provided it is after the bootstrap floor.
func (c AdmissionCheckpoint) Allows(id string, createdAt int64) bool {
	_, consumed := slices.BinarySearch(c.ConsumedPostIDs, id)
	return createdAt > c.FloorMS && createdAt >= c.ReplayFromMS && !consumed
}
