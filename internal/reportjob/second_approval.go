package reportjob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
)

// SecondApproval authorizes one replacement from a specific saved checkpoint.
// The receiver separately verifies the original author's matching new post.
type SecondApproval struct {
	Schema     string `json:"schema"`
	Job        string `json:"job_id"`
	Card       string `json:"card_sha256"`
	Previous   string `json:"previous_approval_sha256"`
	From       string `json:"from_sha256"`
	To         string `json:"to_sha256"`
	FileName   string `json:"file_name"`
	Checkpoint string `json:"checkpoint_sha256"`
	Position   int64  `json:"position"`
	Stage      int    `json:"stage"`
}

func (a SecondApproval) Valid(card Card, first MethodApproval) bool {
	return first.Valid(card) && a.Schema == "orpheus.mattermost.method-approval.v2" &&
		a.Job == card.Job && a.Card == card.Digest() && a.Previous == first.Digest() && a.From == first.To &&
		sha.MatchString(a.To) && a.To != a.From && a.To != first.From &&
		strings.HasSuffix(a.FileName, ".pyz") && attachments.SafeName(a.FileName) == a.FileName &&
		sha.MatchString(a.Checkpoint) && a.Position > 0 && a.Stage >= 2
}

func (a SecondApproval) Digest() string {
	data, _ := json.Marshal(a)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (a SecondApproval) Command(username string) string {
	return "@" + username + " /report-upgrade-from-checkpoint " + a.Job + " " + a.From + " " + a.To +
		" " + strconv.FormatInt(a.Position, 10) + " " + a.Checkpoint
}

func LoadSecondApproval(card Card, first MethodApproval) (SecondApproval, error) {
	path := os.Getenv("MM_REPORT_SECOND_METHOD_APPROVAL")
	if path == "" {
		return SecondApproval{}, os.ErrNotExist
	}
	raw, err := readMethodApproval(path)
	if err != nil {
		return SecondApproval{}, err
	}
	return decodeSecondApproval(raw, card, first)
}

func decodeSecondApproval(raw []byte, card Card, first MethodApproval) (SecondApproval, error) {
	var approval SecondApproval
	if !exactJSONFields(raw, "schema", "job_id", "card_sha256", "previous_approval_sha256", "from_sha256", "to_sha256", "file_name", "checkpoint_sha256", "position", "stage") ||
		json.Unmarshal(raw, &approval) != nil || !approval.Valid(card, first) {
		return SecondApproval{}, errors.New("invalid report checkpoint method approval")
	}
	return approval, nil
}
