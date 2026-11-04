package sandbox

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
)

//go:embed local.py
var localSource []byte

// LocalDigest pins the separate credential-free initialization hook.
func LocalDigest() string { return fmt.Sprintf("%x", sha256.Sum256(localSource)) }

// LocalBeforeRun installs selected local clients. It never imports source files,
// contacts Mattermost or receives the messenger's transport credential.
func LocalBeforeRun() string { return localHook("prepare") }

// LocalAfterRun executes only the service-pinned output exporter, without source access.
func LocalAfterRun() string { return localHook("export") }

func localHook(operation string) string {
	code := fmt.Sprintf(`import base64,hashlib
source=base64.b64decode(%q)
assert hashlib.sha256(source).hexdigest()==%q
exec(compile(source,"local-tools","exec"),{"__name__":"__main__"})
`, base64.StdEncoding.EncodeToString(localSource), LocalDigest())
	return "#!/bin/sh\nset -eu\nexec python3 -I -c '" + code + "' " + operation + "\n"
}
