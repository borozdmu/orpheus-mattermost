package reportjob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
)

// MethodApproval binds one operator-approved method change to the original
// card. A matching post by the original person is verified by the receiver.
type MethodApproval struct {
	Schema   string `json:"schema"`
	Job      string `json:"job_id"`
	Card     string `json:"card_sha256"`
	From     string `json:"from_sha256"`
	To       string `json:"to_sha256"`
	FileName string `json:"file_name"`
}

func (a MethodApproval) Valid(card Card) bool {
	return card.Valid() && a.Schema == "orpheus.mattermost.method-approval.v1" &&
		a.Job == card.Job && a.Card == card.Digest() && a.From == card.Method &&
		sha.MatchString(a.To) && a.To != a.From && strings.HasSuffix(a.FileName, ".pyz") && attachments.SafeName(a.FileName) == a.FileName
}

func (a MethodApproval) Digest() string {
	b, _ := json.Marshal(a)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (a MethodApproval) Command(username string) string {
	return "@" + username + " /report-upgrade " + a.Job + " " + a.From + " " + a.To
}

func MatchesMethodCommand(text, username string, approval MethodApproval) bool {
	return username != "" && strings.TrimSpace(text) == approval.Command(username)
}

func LoadMethodApproval(card Card) (MethodApproval, error) {
	file := os.Getenv("MM_REPORT_METHOD_APPROVAL")
	if file == "" {
		return MethodApproval{}, os.ErrNotExist
	}
	raw, err := readMethodApproval(file)
	if err != nil {
		return MethodApproval{}, err
	}
	return decodeMethodApproval(raw, card)
}

func decodeMethodApproval(raw []byte, card Card) (MethodApproval, error) {
	var approval MethodApproval
	if !exactJSONFields(raw, "schema", "job_id", "card_sha256", "from_sha256", "to_sha256", "file_name") ||
		json.Unmarshal(raw, &approval) != nil || !approval.Valid(card) {
		return MethodApproval{}, errors.New("invalid report method approval")
	}
	return approval, nil
}

func readMethodApproval(file string) ([]byte, error) {
	if !filepath.IsAbs(file) || filepath.Clean(file) != file || file == "/" {
		return nil, errors.New("invalid report method approval path")
	}
	root, err := os.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	// Pin each directory before opening its child. Allow a system sticky
	// directory such as /tmp as an ancestor, never as the approval directory.
	parts := strings.Split(strings.TrimPrefix(file, "/"), "/")
	for i, name := range parts[:len(parts)-1] {
		st, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		stickyAncestor := i < len(parts)-2 && st.Mode()&os.ModeSticky != 0
		if !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0022 != 0 && !stickyAncestor {
			return nil, errors.New("report method approval directory untrusted")
		}
		child, err := root.OpenRoot(name)
		if err != nil {
			return nil, errors.New("report method approval directory changed")
		}
		info, err := child.Stat(".")
		if err != nil || !os.SameFile(st, info) {
			_ = child.Close()
			return nil, errors.New("report method approval directory changed")
		}
		_ = root.Close()
		root = child
	}

	name := parts[len(parts)-1]
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("report method approval must be a regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("report method approval file unavailable")
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(st, info) {
		_ = f.Close()
		return nil, errors.New("report method approval file changed")
	}
	return readCardFile(f)
}
