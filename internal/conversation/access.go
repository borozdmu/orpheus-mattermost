package conversation

import "github.com/orpheus-agents/orpheus/agentcontract"

// AccessProof is the connector's frozen transport evidence. It is stored in
// message metadata, never included in the provider's service scope.
type AccessProof struct {
	Audience         *agentcontract.Audience `json:"audience,omitzero"`
	AudienceSubjects []string                `json:"audience_subjects"`
	ProjectionDigest string                  `json:"projection_digest"`
}
