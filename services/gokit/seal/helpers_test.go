package seal

import (
	"encoding/base64"
	"os"
)

func writeB64(path string, b []byte) error {
	return os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(b)+"\n"), 0o400)
}
