package archive

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	"github.com/xJogger/fake-komga-115/internal/natsort"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/webdav"
)

type ZIPService struct {
	store  *database.Store
	client *oneonefive.Client
	webdav *webdav.Manager
	cache  *cache.Manager
	logger *slog.Logger
}

func NewZIPService(
	store *database.Store,
	client *oneonefive.Client,
	cacheManager *cache.Manager,
	logger *slog.Logger,
) *ZIPService {
	return NewZIPServiceWithWebDAV(store, client, webdav.NewManager(), cacheManager, logger)
}

func NewZIPServiceWithWebDAV(
	store *database.Store,
	client *oneonefive.Client,
	webdavManager *webdav.Manager,
	cacheManager *cache.Manager,
	logger *slog.Logger,
) *ZIPService {
	return &ZIPService{
		store: store, client: client, webdav: webdavManager, cache: cacheManager, logger: logger,
	}
}

func (z *ZIPService) ListPages(ctx context.Context, book database.Book) ([]PageEntry, error) {
	version := bookVersion(book)
	if raw, ok, err := z.store.ArchiveIndexRawForVersion(ctx, book.ID, version); err != nil {
		return nil, err
	} else if ok {
		var pages []PageEntry
		if json.Unmarshal([]byte(raw), &pages) == nil {
			if err := z.ensureEncryptedPagesUnlocked(ctx, book, pages); err != nil {
				return nil, err
			}
			return pages, nil
		}
	}

	started := time.Now()
	blockSize := z.store.Int64Setting(ctx, "range_block_size", 1<<20)
	readerAt, actualSize, err := openBookReaderAtWithSize(ctx, book, z.store, z.client, z.webdav, z.cache, z.logger, blockSize)
	if err != nil {
		diagnostics.Default().Record(diagnostics.Event{
			Level:         diagnostics.LevelError,
			Component:     "archive",
			Stage:         "archive_index",
			Action:        "open",
			Code:          diagnostics.ClassifyError(err),
			DurationMs:    time.Since(started).Milliseconds(),
			CorrelationID: book.ID,
			Detail:        err.Error(),
			Meta:          map[string]any{"format": formatZIP},
		})
		return nil, err
	}
	defer readerAt.Close()
	source := &comicInfoZIPReader{ReaderAt: readerAt}
	reader, err := zip.NewReader(source, actualSize)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		diagnostics.Default().Record(diagnostics.Event{
			Level:         diagnostics.LevelError,
			Component:     "archive",
			Stage:         "archive_index",
			Action:        "parse_zip",
			Code:          "ZIP_ERROR",
			DurationMs:    time.Since(started).Milliseconds(),
			CorrelationID: book.ID,
			Detail:        err.Error(),
			Meta:          map[string]any{"format": formatZIP, "sizeBytes": actualSize},
		})
		return nil, fmt.Errorf("%w: %v", ErrInvalidZIP, err)
	}
	var pages []PageEntry
	var comicInfo *zip.File
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || ignoredEntry(file.Name) {
			continue
		}
		if comicInfo == nil && comicInfoEntry(file.Name) {
			comicInfo = file
		}
		mimeType := MimeType(file.Name)
		if mimeType == "" {
			continue
		}
		encrypted, scheme, realMethod, checkByte, aeVer, cErr := inspectZIPEntryCrypto(file)
		if cErr != nil {
			return nil, cErr
		}
		offset, err := file.DataOffset()
		if err != nil {
			return nil, fmt.Errorf("%w: data offset for %s: %v", ErrInvalidZIP, file.Name, err)
		}
		pages = append(pages, PageEntry{
			Name:             file.Name,
			Format:           formatZIP,
			CompressedSize:   file.CompressedSize64,
			UncompressedSize: file.UncompressedSize64,
			Method:           realMethod,
			MimeType:         mimeType,
			DataOffset:       offset,
			CRC32:            file.CRC32,
			Encrypted:        encrypted,
			EncScheme:        scheme,
			EncCheckByte:     checkByte,
			EncAEVersion:     aeVer,
		})
		if len(pages)%10 == 0 {
			reportLoadPagesFound(ctx, len(pages))
		}
	}
	if hint, ok := ctx.Value(comicInfoHintKey{}).(*comicInfoHint); ok {
		hint.format = formatZIP
		if comicInfo != nil {
			hint.name, hint.zipEntry, hint.zipReader = comicInfo.Name, comicInfo, source
			defer func() {
				// Keep the single entry's local-header offset without retaining
				// the directory or the foreground reader.
				reader.File = nil
				source.ReaderAt = nil
			}()
		}
	}
	reportLoadPagesFound(ctx, len(pages))
	slices.SortFunc(pages, func(a, b PageEntry) int {
		if natsort.Less(a.Name, b.Name) {
			return -1
		}
		if natsort.Less(b.Name, a.Name) {
			return 1
		}
		return 0
	})
	for index := range pages {
		pages[index].Number = index + 1
	}

	if err := z.verifyEncryptedPagesWithReader(ctx, book, pages, readerAt); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(pages)
	if err != nil {
		return nil, err
	}
	if err := z.store.RecordArchiveIndexBuild(
		ctx, book.ID, version, string(encoded), len(pages), time.Since(started),
	); err != nil {
		return nil, err
	}
	diagnostics.Default().RecordVerbose(diagnostics.Event{
		Level:         diagnostics.LevelDebug,
		Component:     "archive",
		Stage:         "archive_index",
		Action:        "parse_zip",
		DurationMs:    time.Since(started).Milliseconds(),
		CorrelationID: book.ID,
		Meta:          map[string]any{"format": formatZIP, "pages": len(pages)},
	})
	z.logger.Info("zip index parsed", "book", book.ID, "pages", len(pages))
	return pages, nil
}

func firstEncryptedPageEntry(pages []PageEntry) (PageEntry, bool) {
	for _, p := range pages {
		if p.Encrypted {
			return p, true
		}
	}
	return PageEntry{}, false
}

func (z *ZIPService) ensureEncryptedPagesUnlocked(ctx context.Context, book database.Book, pages []PageEntry) error {
	sample, hasEnc := firstEncryptedPageEntry(pages)
	if !hasEnc {
		return nil
	}
	cands, hasExplicit, err := resolveArchivePasswordCandidates(ctx, z.store, book)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		if hasExplicit {
			return ErrArchivePasswordInvalid
		}
		return ErrArchivePasswordRequired
	}
	// Fast path: already has a direct password binding and not running explicit verification.
	if !hasExplicit && cands[0].Source == "direct" {
		return nil
	}
	maxPageSize := z.maxPageSize(ctx)
	hasDirect := false
	for _, cand := range cands {
		if cand.Source == "direct" {
			hasDirect = true
		}
		if _, err := z.readEntryWithPassword(ctx, book, sample, maxPageSize, cand.Password); err == nil {
			maybeAutoBindArchivePassword(ctx, z.store, book, cand)
			return nil
		} else if !isArchivePasswordError(err) {
			return err
		}
	}
	if hasExplicit || hasDirect {
		return ErrArchivePasswordInvalid
	}
	return ErrArchivePasswordRequired
}

func (z *ZIPService) verifyEncryptedPagesWithReader(ctx context.Context, book database.Book, pages []PageEntry, readerAt ReaderAtCloser) error {
	sample, hasEnc := firstEncryptedPageEntry(pages)
	if !hasEnc {
		return nil
	}
	cands, hasExplicit, err := resolveArchivePasswordCandidates(ctx, z.store, book)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		if hasExplicit {
			return ErrArchivePasswordInvalid
		}
		return ErrArchivePasswordRequired
	}
	maxPageSize := z.maxPageSize(ctx)
	if sample.UncompressedSize > uint64(maxPageSize) || sample.CompressedSize > uint64(maxPageSize) {
		return ErrPageTooLarge
	}
	compressed := make([]byte, int(sample.CompressedSize))
	if _, err := readerAt.ReadAt(compressed, sample.DataOffset); err != nil {
		return err
	}
	hasDirect := false
	for _, cand := range cands {
		if cand.Source == "direct" {
			hasDirect = true
		}
		if _, err := decodeZIPEntryBytes(compressed, sample, maxPageSize, cand.Password); err == nil {
			maybeAutoBindArchivePassword(ctx, z.store, book, cand)
			return nil
		} else if !isArchivePasswordError(err) {
			return err
		}
	}
	if hasExplicit || hasDirect {
		return ErrArchivePasswordInvalid
	}
	return ErrArchivePasswordRequired
}

func (z *ZIPService) maxPageSize(ctx context.Context) int64 {
	maxPageSize := z.store.Int64Setting(ctx, "max_page_size", 100<<20)
	if maxPageSize <= 0 {
		maxPageSize = 100 << 20
	}
	return maxPageSize
}

func (z *ZIPService) ReadPage(ctx context.Context, book database.Book, pageNumber int) (PageData, error) {
	pages, err := z.ListPages(ctx, book)
	if err != nil {
		return PageData{}, err
	}
	if pageNumber < 1 || pageNumber > len(pages) {
		return PageData{}, sql.ErrNoRows
	}
	entry := pages[pageNumber-1]
	maxPageSize := z.maxPageSize(ctx)
	if entry.UncompressedSize > uint64(maxPageSize) ||
		entry.CompressedSize > uint64(maxPageSize) {
		return PageData{}, ErrPageTooLarge
	}
	_, hasExplicit := explicitArchivePasswordFromContext(ctx)
	var cands []database.ArchivePasswordCandidate
	if entry.Encrypted {
		var cErr error
		cands, hasExplicit, cErr = resolveArchivePasswordCandidates(ctx, z.store, book)
		if cErr != nil {
			return PageData{}, cErr
		}
		if len(cands) == 0 {
			if hasExplicit {
				return PageData{}, ErrArchivePasswordInvalid
			}
			return PageData{}, ErrArchivePasswordRequired
		}
	}
	loader := func(loadCtx context.Context) ([]byte, error) {
		if !entry.Encrypted {
			return z.readEntryWithPassword(loadCtx, book, entry, maxPageSize, "")
		}
		hasDirect := false
		for _, cand := range cands {
			if cand.Source == "direct" {
				hasDirect = true
			}
			data, err := z.readEntryWithPassword(loadCtx, book, entry, maxPageSize, cand.Password)
			if err == nil {
				maybeAutoBindArchivePassword(loadCtx, z.store, book, cand)
				return data, nil
			}
			if !isArchivePasswordError(err) {
				return nil, err
			}
		}
		if hasExplicit || hasDirect {
			return nil, ErrArchivePasswordInvalid
		}
		return nil, ErrArchivePasswordRequired
	}
	if hasExplicit {
		data, err := loader(ctx)
		return PageData{Entry: entry, Data: data, TotalPages: len(pages)}, err
	}
	pageKey := fmt.Sprintf("%s:%s:%d", book.ID, bookVersion(book), pageNumber)
	if entry.Method == zip.Deflate || entry.Encrypted {
		data, _, err := z.cache.GetOrLoad(
			ctx, cache.TypePage, pageKey,
			z.store.Int64Setting(ctx, "page_cache_max_bytes", 5<<30),
			loader,
		)
		return PageData{Entry: entry, Data: data, TotalPages: len(pages)}, err
	}
	data, err := loader(ctx)
	return PageData{Entry: entry, Data: data, TotalPages: len(pages)}, err
}

func (z *ZIPService) Prefetch(book database.Book, afterPage, count int) {
	if count <= 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for page := afterPage + 1; page <= afterPage+count; page++ {
			if _, err := z.ReadPage(ctx, book, page); err != nil {
				return
			}
		}
	}()
}

func (z *ZIPService) readEntryWithPassword(
	ctx context.Context,
	book database.Book,
	entry PageEntry,
	maxPageSize int64,
	password string,
) ([]byte, error) {
	compressed := make([]byte, int(entry.CompressedSize))
	reportLoadBytes(ctx, 0, int64(entry.CompressedSize))
	blockSize := z.store.Int64Setting(ctx, "range_block_size", 1<<20)
	readerAt, err := openBookReaderAt(ctx, book, z.store, z.client, z.webdav, z.cache, z.logger, blockSize)
	if err != nil {
		return nil, err
	}
	defer readerAt.Close()
	if _, err := readerAt.ReadAt(compressed, entry.DataOffset); err != nil {
		return nil, err
	}
	reportLoadStage(ctx, "decompressing", "正在解压页面图像…")
	return decodeZIPEntryBytes(compressed, entry, maxPageSize, password)
}

func decodeZIPEntryBytes(
	compressed []byte,
	entry PageEntry,
	maxPageSize int64,
	password string,
) ([]byte, error) {
	payload, err := decryptZIPEntryPayload(compressed, entry, password)
	if err != nil {
		return nil, err
	}
	var data []byte
	switch entry.Method {
	case zip.Store:
		data = payload
	case zip.Deflate:
		reader := flate.NewReader(bytes.NewReader(payload))
		defer reader.Close()
		var err error
		data, err = io.ReadAll(io.LimitReader(reader, maxPageSize+1))
		if err != nil {
			if entry.Encrypted {
				return nil, ErrArchivePasswordInvalid
			}
			return nil, err
		}
		if int64(len(data)) > maxPageSize {
			return nil, ErrPageTooLarge
		}
	default:
		return nil, ErrUnsupportedZIP
	}
	if uint64(len(data)) != entry.UncompressedSize {
		if entry.Encrypted {
			return nil, ErrArchivePasswordInvalid
		}
		return nil, fmt.Errorf("%w: page checksum or size mismatch", ErrInvalidZIP)
	}
	// WinZip AE-2 (and AE-1 for small/omitted entries) sets CRC-32 to 0 and relies
	// on HMAC-SHA1-80 authentication already verified above.
	verifyCRC := !(entry.Encrypted && entry.EncAEVersion >= 1 && entry.CRC32 == 0)
	if verifyCRC && crc32.ChecksumIEEE(data) != entry.CRC32 {
		if entry.Encrypted {
			return nil, ErrArchivePasswordInvalid
		}
		return nil, fmt.Errorf("%w: page checksum or size mismatch", ErrInvalidZIP)
	}
	return data, nil
}

func MimeType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".avif":
		return "image/avif"
	default:
		return ""
	}
}

func ignoredEntry(name string) bool {
	clean := strings.ReplaceAll(name, "\\", "/")
	base := strings.ToLower(filepath.Base(clean))
	return strings.HasPrefix(clean, "__MACOSX/") ||
		base == ".ds_store" ||
		base == "thumbs.db"
}

func bookVersion(book database.Book) string {
	modified := ""
	if book.FileModifiedAt != nil {
		modified = book.FileModifiedAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf(
		"%s:%s:%d:%s:%s",
		book.FileID, archiveFormat(book.Name), book.Size, book.SHA1, modified,
	)
}

func BookVersion(book database.Book) string {
	return bookVersion(book)
}
