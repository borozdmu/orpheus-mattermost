package sandbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLocalHookDefaultBytesUnchanged(t *testing.T) {
	code := fmt.Sprintf(`import base64,hashlib
source=base64.b64decode(%q)
assert hashlib.sha256(source).hexdigest()==%q
exec(compile(source,"local-tools","exec"),{"__name__":"__main__"})
`, base64.StdEncoding.EncodeToString(localSource), LocalDigest())
	prefix := "#!/bin/sh\nset -eu\nexec python3 -I -c '" + code + "' "
	if LocalBeforeRun("") != prefix+"prepare\n" || LocalAfterRun() != prefix+"export\n" {
		t.Fatal("default local hooks changed")
	}
}

func TestLocalBootstrapRunsAfterPreparationAndFailsClosed(t *testing.T) {
	for _, scenario := range []string{"valid", "prepare failure", "bootstrap failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			source := []byte("print('client must not execute')")
			raw, err := json.Marshal(map[string]any{"directory": ".agent-memory", "source_b64": base64.StdEncoding.EncodeToString(source), "sha256": fmt.Sprintf("%x", sha256.Sum256(source))})
			if err != nil {
				t.Fatal(err)
			}
			script := `import os,json
from pathlib import Path
p=Path(os.environ['ORPHEUS_WORKSPACE_PATH'])
assert (p/'.agent-memory/client.py').read_text()=="print('client must not execute')"
assert json.loads((p/'.agent-memory/current-run.json').read_text())['run_id']==os.environ['ORPHEUS_RUN_ID']
(p/'bootstrap-done').write_text("safe ' quotes ; $()")
`
			run := "22222222-2222-4222-8222-222222222222"
			if scenario == "prepare failure" {
				run = "invalid"
			}
			if scenario == "bootstrap failure" {
				script = "raise RuntimeError('synthetic bootstrap failure')\n" + script
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", LocalBeforeRun(script))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ORPHEUS_WORKSPACE_PATH=" + dir, "ORPHEUS_SESSION_ID=11111111-1111-4111-8111-111111111111", "ORPHEUS_RUN_ID=" + run, "ORPHEUS_LOCAL_TOOL_MEMORY=" + base64.StdEncoding.EncodeToString(raw)}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("hook: %v %s", err, out)
			}
			_, err = os.Stat(filepath.Join(dir, "bootstrap-done"))
			if (err == nil) != (scenario == "valid") {
				t.Fatal("bootstrap ran after failed preparation or failure was swallowed")
			}
		})
	}
}
