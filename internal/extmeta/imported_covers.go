package extmeta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"github.com/xJogger/fake-komga-115/internal/importedcover"
	_ "golang.org/x/image/webp"
)

const maxImportedCoverPixels = 24_000_000

func writeImportedCover(root string, raw []byte) (string, error) {
	if len(raw) > maxSQLiteBlobCoverBytes {
		return "", ErrInvalidImage
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > maxImportedCoverPixels/config.Height {
		return "", ErrInvalidImage
	}
	switch format {
	case "jpeg":
		format = "jpg"
	case "png", "gif", "webp":
	default:
		return "", ErrInvalidImage
	}
	ref := fmt.Sprintf("%x.%s", sha256.Sum256(raw), format)
	path, ok := importedcover.Path(root, ref)
	if !ok {
		return "", ErrInvalidConfig
	}
	if _, err := os.Stat(path); err == nil {
		return ref, nil
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		return "", ErrInvalidImage
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cover-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return ref, nil
}

// The manager serializes candidate commits and collection of unreferenced files.
func (m *Manager) removeUnusedImportedCovers(ctx context.Context, refs []string) {
	ctx = context.WithoutCancel(ctx)
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		path, ok := importedcover.Path(m.importedCovers, ref)
		if !ok {
			continue
		}
		used, err := m.store.IsImportedCoverRefReferenced(ctx, ref)
		if err == nil && !used {
			_ = os.Remove(path)
		}
	}
}

// RemoveUnusedImportedCovers is called after a catalog transaction deletes candidates.
func (m *Manager) RemoveUnusedImportedCovers(ctx context.Context, refs []string) {
	m.importedMu.Lock()
	defer m.importedMu.Unlock()
	m.removeUnusedImportedCovers(ctx, refs)
}
