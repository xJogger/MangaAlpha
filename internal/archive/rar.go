package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nwaples/rardecode/v2"

	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/diagnostics"
	"github.com/xJogger/fake-komga-115/internal/natsort"
	"github.com/xJogger/fake-komga-115/internal/oneonefive"
	"github.com/xJogger/fake-komga-115/internal/webdav"
)

const remoteRARName = "archive.rar"

type RARService struct {
	store  *database.Store
	client *oneonefive.Client
	webdav *webdav.Manager
	cache  *cache.Manager
	logger *slog.Logger
}

func NewRARService(
	store *database.Store,
	client *oneonefive.Client,
	cacheManager *cache.Manager,
	logger *slog.Logger,
) *RARService {
	return NewRARServiceWithWebDAV(store, client, webdav.NewManager(), cacheManager, logger)
}

func NewRARServiceWithWebDAV(
	store *database.Store,
	client *oneonefive.Client,
	webdavManager *webdav.Manager,
	cacheManager *cache.Manager,
	logger *slog.Logger,
) *RARService {
	return &RARService{
		store: store, client: client, webdav: webdavManager, cache: cacheManager, logger: logger,
	}
}

func (r *RARService) maxPageSize(ctx context.Context) int64 {
	maxPageSize := r.store.Int64Setting(ctx, "max_page_size", 100<<20)
	if maxPageSize <= 0 {
		maxPageSize = 100 << 20
	}
	return maxPageSize
}

func (r *RARService) ListPages(ctx context.Context, book database.Book) ([]PageEntry, error) {
	version := bookVersion(book)
	if raw, ok, err := r.store.ArchiveIndexRawForVersion(ctx, book.ID, version); err != nil {
		return nil, err
	} else if ok {
		var pages []PageEntry
		if json.Unmarshal([]byte(raw), &pages) == nil {
			if err := r.ensureEncryptedPagesUnlocked(ctx, book, pages); err != nil {
				return nil, err
			}
			return pages, nil
		}
	}

	started := time.Now()
	cands, hasExplicit, err := resolveArchivePasswordCandidates(ctx, r.store, book)
	if err != nil {
		return nil, err
	}

	var (
		pages    []PageEntry
		parseErr error
	)
	if len(cands) == 0 {
		files, remote, err := r.listFilesWithPassword(ctx, book, "")
		if err != nil {
			if hasExplicit && isArchivePasswordError(err) {
				return nil, ErrArchivePasswordInvalid
			}
			if !isArchivePasswordError(err) {
				r.recordRARError(book, started, "parse_rar", err)
			}
			return nil, err
		}
		pages, err = rarPageEntriesWithContext(ctx, files)
		if err != nil {
			r.recordRARError(book, started, "validate_rar", err)
			return nil, err
		}
		attachRARComicInfoReader(ctx, remote, false)
		if _, hasEnc := firstEncryptedPageEntry(pages); hasEnc {
			if hasExplicit {
				return nil, ErrArchivePasswordInvalid
			}
			return nil, ErrArchivePasswordRequired
		}
	} else {
		maxPageSize := r.maxPageSize(ctx)
		hasDirect := false
		unlocked := false
		for _, cand := range cands {
			if cand.Source == "direct" {
				hasDirect = true
			}
			files, remoteFS, lErr := r.listFilesWithPassword(ctx, book, cand.Password)
			if lErr != nil {
				if isArchivePasswordError(lErr) {
					continue
				}
				parseErr = lErr
				break
			}
			candPages, vErr := rarPageEntriesWithContext(ctx, files)
			if vErr != nil {
				parseErr = vErr
				break
			}
			sample, hasEnc := firstEncryptedPageEntry(candPages)
			if !hasEnc {
				attachRARComicInfoReader(ctx, remoteFS, true)
				pages = candPages
				unlocked = true
				break
			}
			if sample.UncompressedSize > uint64(maxPageSize) || sample.CompressedSize > uint64(maxPageSize) {
				parseErr = ErrPageTooLarge
				break
			}
			if _, rErr := readRARFileEntry(remoteFS, files, sample, maxPageSize, true); rErr != nil {
				if isArchivePasswordError(rErr) {
					continue
				}
				parseErr = rErr
				break
			}
			maybeAutoBindArchivePassword(ctx, r.store, book, cand)
			attachRARComicInfoReader(ctx, remoteFS, true)
			pages = candPages
			unlocked = true
			break
		}
		if !unlocked {
			if parseErr != nil {
				r.recordRARError(book, started, "parse_rar", parseErr)
				return nil, parseErr
			}
			if hasExplicit || hasDirect {
				return nil, ErrArchivePasswordInvalid
			}
			return nil, ErrArchivePasswordRequired
		}
	}

	reportLoadPagesFound(ctx, len(pages))
	if err := r.saveIndex(ctx, book, pages, time.Since(started)); err != nil {
		return nil, err
	}
	diagnostics.Default().RecordVerbose(diagnostics.Event{
		Level:         diagnostics.LevelDebug,
		Component:     "archive",
		Stage:         "archive_index",
		Action:        "parse_rar",
		DurationMs:    time.Since(started).Milliseconds(),
		CorrelationID: book.ID,
		Meta:          map[string]any{"format": formatRAR, "pages": len(pages)},
	})
	r.logger.Info("rar index parsed", "book", book.ID, "pages", len(pages))
	return pages, nil
}

func (r *RARService) recordRARError(book database.Book, started time.Time, action string, err error) {
	diagnostics.Default().Record(diagnostics.Event{
		Level:         diagnostics.LevelError,
		Component:     "archive",
		Stage:         "archive_index",
		Action:        action,
		Code:          diagnostics.ClassifyError(err),
		DurationMs:    time.Since(started).Milliseconds(),
		CorrelationID: book.ID,
		Detail:        err.Error(),
		Meta:          map[string]any{"format": formatRAR, "sizeBytes": book.Size},
	})
}

func (r *RARService) ensureEncryptedPagesUnlocked(ctx context.Context, book database.Book, pages []PageEntry) error {
	sample, hasEnc := firstEncryptedPageEntry(pages)
	if !hasEnc {
		return nil
	}
	cands, hasExplicit, err := resolveArchivePasswordCandidates(ctx, r.store, book)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		if hasExplicit {
			return ErrArchivePasswordInvalid
		}
		return ErrArchivePasswordRequired
	}
	if !hasExplicit && cands[0].Source == "direct" {
		return nil
	}
	maxPageSize := r.maxPageSize(ctx)
	hasDirect := false
	for _, cand := range cands {
		if cand.Source == "direct" {
			hasDirect = true
		}
		if _, err := r.readEntryWithPassword(ctx, book, sample, maxPageSize, cand.Password); err == nil {
			maybeAutoBindArchivePassword(ctx, r.store, book, cand)
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

func rarPageEntries(files []*rardecode.File) ([]PageEntry, error) {
	return rarPageEntriesWithContext(context.Background(), files)
}

func rarPageEntriesWithContext(ctx context.Context, files []*rardecode.File) ([]PageEntry, error) {
	var comicInfo *rardecode.File
	pages := make([]PageEntry, 0, len(files))
	for index, file := range files {
		if err := validateRARFile(file); err != nil {
			return nil, err
		}
		if file.IsDir || ignoredEntry(file.Name) {
			continue
		}
		if comicInfo == nil && comicInfoEntry(file.Name) {
			comicInfo = file
		}
		mimeType := MimeType(file.Name)
		if mimeType == "" {
			continue
		}
		encrypted := file.Encrypted || file.HeaderEncrypted
		scheme := ""
		if encrypted {
			scheme = encSchemeRAR
		}
		pages = append(pages, PageEntry{
			Name:             file.Name,
			Format:           formatRAR,
			ArchiveIndex:     index,
			CompressedSize:   uint64(file.PackedSize),
			UncompressedSize: uint64(file.UnPackedSize),
			MimeType:         mimeType,
			Encrypted:        encrypted,
			EncScheme:        scheme,
		})
	}
	if hint, ok := ctx.Value(comicInfoHintKey{}).(*comicInfoHint); ok {
		hint.format, hint.rarEntry = formatRAR, comicInfo
		if comicInfo != nil {
			hint.name = comicInfo.Name
		}
	}
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
	return pages, nil
}

func attachRARComicInfoReader(ctx context.Context, remote *remoteArchiveFS, hasPassword bool) {
	if hint, ok := ctx.Value(comicInfoHintKey{}).(*comicInfoHint); ok && hint.rarEntry != nil {
		hint.rarFS, hint.rarPassword = remote, hasPassword
	}
}

func (r *RARService) ReadPage(
	ctx context.Context,
	book database.Book,
	pageNumber int,
) (PageData, error) {
	pages, err := r.ListPages(ctx, book)
	if err != nil {
		return PageData{}, err
	}
	if pageNumber < 1 || pageNumber > len(pages) {
		return PageData{}, sql.ErrNoRows
	}
	entry := pages[pageNumber-1]
	maxPageSize := r.maxPageSize(ctx)
	if entry.UncompressedSize > uint64(maxPageSize) ||
		entry.CompressedSize > uint64(maxPageSize) {
		return PageData{}, ErrPageTooLarge
	}
	_, hasExplicit := explicitArchivePasswordFromContext(ctx)
	var cands []database.ArchivePasswordCandidate
	if entry.Encrypted {
		var cErr error
		cands, hasExplicit, cErr = resolveArchivePasswordCandidates(ctx, r.store, book)
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
			return r.readEntryWithPassword(loadCtx, book, entry, maxPageSize, "")
		}
		hasDirect := false
		for _, cand := range cands {
			if cand.Source == "direct" {
				hasDirect = true
			}
			data, err := r.readEntryWithPassword(loadCtx, book, entry, maxPageSize, cand.Password)
			if err == nil {
				maybeAutoBindArchivePassword(loadCtx, r.store, book, cand)
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
	data, _, err := r.cache.GetOrLoad(
		ctx, cache.TypePage, pageKey,
		r.store.Int64Setting(ctx, "page_cache_max_bytes", 5<<30),
		loader,
	)
	return PageData{Entry: entry, Data: data, TotalPages: len(pages)}, err
}

func (r *RARService) Prefetch(book database.Book, afterPage, count int) {
	if count <= 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for page := afterPage + 1; page <= afterPage+count; page++ {
			if _, err := r.ReadPage(ctx, book, page); err != nil {
				return
			}
		}
	}()
}

func (r *RARService) listFiles(
	ctx context.Context,
	book database.Book,
) ([]*rardecode.File, *remoteArchiveFS, error) {
	return r.listFilesWithPassword(ctx, book, "")
}

func (r *RARService) listFilesWithPassword(
	ctx context.Context,
	book database.Book,
	password string,
) ([]*rardecode.File, *remoteArchiveFS, error) {
	indexBlockSize := r.store.Int64Setting(ctx, "rar_index_block_size", 64<<10)
	dataBlockSize := r.store.Int64Setting(ctx, "range_block_size", 1<<20)
	remoteFS := newRemoteArchiveFS(
		ctx, book, r.store, r.client, r.webdav, r.cache, r.logger, indexBlockSize, dataBlockSize,
	)
	maxDictionarySize := r.store.Int64Setting(ctx, "rar_max_dictionary_size", 100<<20)
	if maxDictionarySize < 256<<10 {
		maxDictionarySize = 256 << 10
	}
	opts := []rardecode.Option{
		rardecode.FileSystem(remoteFS),
		rardecode.BufferSize(32 << 10),
		rardecode.MaxDictionarySize(maxDictionarySize),
	}
	if password != "" {
		opts = append(opts, rardecode.Password(password))
	}
	files, err := rardecode.List(remoteRARName, opts...)
	if err != nil {
		if remoteFS.lastOpenErr != nil {
			return nil, remoteFS, remoteFS.lastOpenErr
		}
		return nil, remoteFS, mapRARErrorWithPassword(err, password != "")
	}
	return files, remoteFS, nil
}

func (r *RARService) readEntry(
	ctx context.Context,
	book database.Book,
	entry PageEntry,
	maxPageSize int64,
) ([]byte, error) {
	return r.readEntryWithPassword(ctx, book, entry, maxPageSize, "")
}

func (r *RARService) readEntryWithPassword(
	ctx context.Context,
	book database.Book,
	entry PageEntry,
	maxPageSize int64,
	password string,
) ([]byte, error) {
	files, remoteFS, err := r.listFilesWithPassword(ctx, book, password)
	if err != nil {
		return nil, err
	}
	return readRARFileEntry(remoteFS, files, entry, maxPageSize, password != "")
}

func readRARFileEntry(
	remoteFS *remoteArchiveFS,
	files []*rardecode.File,
	entry PageEntry,
	maxPageSize int64,
	hasPassword bool,
) ([]byte, error) {
	if entry.ArchiveIndex < 0 || entry.ArchiveIndex >= len(files) {
		return nil, fmt.Errorf("%w: page entry no longer exists", ErrInvalidRAR)
	}
	return readRARFile(remoteFS, files[entry.ArchiveIndex], entry, maxPageSize, hasPassword)
}

func readRARFile(remoteFS *remoteArchiveFS, file *rardecode.File, entry PageEntry, maxPageSize int64, hasPassword bool) ([]byte, error) {
	remoteFS.UseDataBlocks()
	if err := validateRARFile(file); err != nil {
		return nil, err
	}
	if file.Name != entry.Name || uint64(file.UnPackedSize) != entry.UncompressedSize {
		return nil, fmt.Errorf("%w: page entry changed", ErrInvalidRAR)
	}
	isEnc := file.Encrypted || file.HeaderEncrypted || entry.Encrypted
	reader, err := file.Open()
	if err != nil {
		return nil, mapRARErrorWithPassword(err, hasPassword && isEnc)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxPageSize+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, mapRARErrorWithPassword(readErr, hasPassword && isEnc)
	}
	if closeErr != nil {
		return nil, mapRARErrorWithPassword(closeErr, hasPassword && isEnc)
	}
	if int64(len(data)) > maxPageSize {
		return nil, ErrPageTooLarge
	}
	if uint64(len(data)) != entry.UncompressedSize {
		if hasPassword && isEnc {
			return nil, ErrArchivePasswordInvalid
		}
		return nil, fmt.Errorf("%w: page size mismatch", ErrInvalidRAR)
	}
	return data, nil
}

func (r *RARService) saveIndex(
	ctx context.Context,
	book database.Book,
	pages []PageEntry,
	duration time.Duration,
) error {
	encoded, err := json.Marshal(pages)
	if err != nil {
		return err
	}
	return r.store.RecordArchiveIndexBuild(
		ctx, book.ID, bookVersion(book), string(encoded), len(pages), duration,
	)
}

func validateRARFile(file *rardecode.File) error {
	switch {
	case !file.IsDir && file.Solid:
		return ErrSolidRAR
	case !file.IsDir && file.UnKnownSize:
		return fmt.Errorf("%w: entry %q has unknown unpacked size", ErrUnsupportedRAR, file.Name)
	case file.PackedSize < 0 || file.UnPackedSize < 0:
		return fmt.Errorf("%w: entry %q has an invalid size", ErrInvalidRAR, file.Name)
	default:
		return nil
	}
}

func mapRARError(err error) error {
	return mapRARErrorWithPassword(err, false)
}

func mapRARErrorWithPassword(err error, hasPassword bool) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrSolidRAR),
		errors.Is(err, ErrEncryptedRAR),
		errors.Is(err, ErrArchivePasswordRequired),
		errors.Is(err, ErrArchivePasswordInvalid),
		errors.Is(err, ErrMultiVolumeRAR),
		errors.Is(err, ErrUnsupportedRAR),
		errors.Is(err, ErrInvalidRAR):
		return err
	case errors.Is(err, rardecode.ErrSolidOpen):
		return ErrSolidRAR
	case errors.Is(err, rardecode.ErrBadPassword):
		return ErrArchivePasswordInvalid
	case errors.Is(err, rardecode.ErrArchiveEncrypted),
		errors.Is(err, rardecode.ErrArchivedFileEncrypted):
		if hasPassword {
			return ErrArchivePasswordInvalid
		}
		return ErrArchivePasswordRequired
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: a required volume is unavailable", ErrMultiVolumeRAR)
	case errors.Is(err, rardecode.ErrDictionaryTooLarge):
		return fmt.Errorf("%w: decompression dictionary exceeds the configured limit", ErrUnsupportedRAR)
	default:
		if hasPassword {
			// RAR4 has no dedicated header password check hash; an incorrect password on
			// an encrypted RAR4 archive surfaces as a header CRC, file checksum, or decode error.
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "password") ||
				strings.Contains(msg, "checksum") ||
				strings.Contains(msg, "corrupt") ||
				strings.Contains(msg, "bad header") ||
				strings.Contains(msg, "decode") ||
				strings.Contains(msg, "unexpected eof") {
				return ErrArchivePasswordInvalid
			}
		}
		return fmt.Errorf("%w: %v", ErrInvalidRAR, err)
	}
}

type remoteArchiveFS struct {
	ctx         context.Context
	book        database.Book
	store       *database.Store
	client      *oneonefive.Client
	webdav      *webdav.Manager
	cache       *cache.Manager
	logger      *slog.Logger
	indexSize   int64
	dataSize    int64
	dataMode    atomic.Bool
	lastOpenErr error
}

func newRemoteArchiveFS(
	ctx context.Context,
	book database.Book,
	store *database.Store,
	client *oneonefive.Client,
	webdavManager *webdav.Manager,
	cacheManager *cache.Manager,
	logger *slog.Logger,
	indexSize, dataSize int64,
) *remoteArchiveFS {
	if indexSize <= 0 {
		indexSize = 64 << 10
	}
	if dataSize <= 0 {
		dataSize = 1 << 20
	}
	return &remoteArchiveFS{
		ctx: ctx, book: book, store: store, client: client, webdav: webdavManager,
		cache: cacheManager, logger: logger, indexSize: indexSize, dataSize: dataSize,
	}
}

func (r *remoteArchiveFS) UseDataBlocks() {
	r.dataMode.Store(true)
}

func (r *remoteArchiveFS) Open(name string) (fs.File, error) {
	clean := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "./")
	if clean != remoteRARName {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	blockSize := r.indexSize
	if r.dataMode.Load() {
		blockSize = r.dataSize
	}
	reader, actualSize, err := openBookReaderAtWithSize(
		r.ctx, r.book, r.store, r.client, r.webdav, r.cache, r.logger, blockSize,
	)
	if err != nil {
		r.lastOpenErr = err
		return nil, err
	}
	return &remoteArchiveFile{reader: reader, size: actualSize, name: remoteRARName}, nil
}

type remoteArchiveFile struct {
	reader ReaderAtCloser
	size   int64
	offset int64
	name   string
}

func (r *remoteArchiveFile) Read(p []byte) (int, error) {
	n, err := r.reader.ReadAt(p, r.offset)
	r.offset += int64(n)
	return n, err
}

func (r *remoteArchiveFile) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.offset + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, fs.ErrInvalid
	}
	if next < 0 {
		return 0, fs.ErrInvalid
	}
	r.offset = next
	return next, nil
}

func (r *remoteArchiveFile) Stat() (fs.FileInfo, error) {
	return remoteArchiveFileInfo{name: r.name, size: r.size}, nil
}

func (r *remoteArchiveFile) Close() error {
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}

type remoteArchiveFileInfo struct {
	name string
	size int64
}

func (r remoteArchiveFileInfo) Name() string       { return r.name }
func (r remoteArchiveFileInfo) Size() int64        { return r.size }
func (r remoteArchiveFileInfo) Mode() fs.FileMode  { return 0o444 }
func (r remoteArchiveFileInfo) ModTime() time.Time { return time.Time{} }
func (r remoteArchiveFileInfo) IsDir() bool        { return false }
func (r remoteArchiveFileInfo) Sys() any           { return nil }
