package reportjob

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
)

var errOutput = errors.New("invalid report output")

// Inspect only a small deterministic receipt, without extracting any archive
// member. The pinned calculation separately validates its normalized events.
func CheckpointPosition(data []byte, j Stage) (int64, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(z.File) > 4096 {
		return 0, errOutput
	}
	names := map[string]bool{}
	var receipt *zip.File
	var size uint64
	for _, f := range z.File {
		if !fs.ValidPath(f.Name) || names[f.Name] || strings.Contains(f.Name, "\\") || !f.Mode().IsRegular() || f.UncompressedSize64 > 32<<20 {
			return 0, errOutput
		}
		size += f.UncompressedSize64
		if size > 32<<20 {
			return 0, errOutput
		}
		names[f.Name] = true
		if f.Name == "job-progress.json" {
			receipt = f
		}
	}
	if receipt == nil || receipt.UncompressedSize64 > 1024 || len(z.File) < 2 {
		return 0, errOutput
	}
	f, err := receipt.Open()
	if err != nil {
		return 0, errOutput
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(raw) > 1024 {
		return 0, errOutput
	}
	var witness struct {
		Schema   string `json:"schema"`
		Job      string `json:"job_id"`
		Stage    int    `json:"stage"`
		Position int64  `json:"position"`
	}
	if !json.Valid(raw) || !exactJSONFields(raw, "schema", "job_id", "stage", "position") || json.Unmarshal(raw, &witness) != nil || witness.Schema != "orpheus.report-progress.v1" || witness.Job != j.Job || witness.Stage != j.Number || witness.Position <= 0 || j.Prior != nil && witness.Position <= j.Prior.Position {
		return 0, errOutput
	}
	return witness.Position, nil
}

func exactJSONFields(raw []byte, fields ...string) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	wanted := map[string]bool{}
	for _, k := range fields {
		wanted[k] = true
	}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || !wanted[name] || d.Decode(new(json.RawMessage)) != nil {
			return false
		}
		delete(wanted, name)
	}
	end, err := d.Token()
	return err == nil && end == json.Delim('}') && len(wanted) == 0 && d.Decode(new(any)) == io.EOF
}

func Workbook(data []byte) bool {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	content, workbook, sheet := false, false, false
	for _, f := range z.File {
		switch f.Name {
		case "[Content_Types].xml":
			content = f.UncompressedSize64 > 0
		case "xl/workbook.xml":
			workbook = f.UncompressedSize64 > 0
		default:
			sheet = sheet || strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml") && f.UncompressedSize64 > 0
		}
	}
	return content && workbook && sheet
}
