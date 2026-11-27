package importedcover

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var importedCoverRef = regexp.MustCompile(`^[0-9a-f]{64}\.(jpg|png|webp|gif)$`)

// ValidRef reports whether ref is a supported content-addressed image name.
func ValidRef(ref string) bool { return importedCoverRef.MatchString(ref) }

func Path(root, ref string) (string, bool) {
	if root == "" || !ValidRef(ref) {
		return "", false
	}
	return filepath.Join(root, ref[:2], ref), true
}

// Read reads an image validated and copied from the source at match time.
func Read(root, ref string) ([]byte, string, error) {
	path, ok := Path(root, ref)
	if !ok {
		return nil, "", fs.ErrInvalid
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	ext := strings.TrimPrefix(filepath.Ext(ref), ".")
	if ext == "jpg" {
		ext = "jpeg"
	}
	return raw, "image/" + ext, nil
}
