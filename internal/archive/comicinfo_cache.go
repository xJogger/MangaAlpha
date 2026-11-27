package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwaples/rardecode/v2"

	"github.com/xJogger/fake-komga-115/internal/database"
)

const (
	ComicInfoParsed             = "parsed"
	ComicInfoAbsent             = "absent"
	ComicInfoSkippedSolid       = "skipped_solid"
	ComicInfoSkippedEncrypted   = "skipped_encrypted"
	ComicInfoSkippedUnsupported = "skipped_unsupported"
	maxComicInfoSize            = 512 << 10
)

// ComicInfoParser converts a bounded archive entry into catalog fields.
type ComicInfoParser func(io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error)

type comicInfoHint struct {
	format      string
	name        string
	status      string
	zipEntry    *zip.File
	zipReader   *comicInfoZIPReader
	rarEntry    *rardecode.File
	rarFS       *remoteArchiveFS
	rarPassword bool
}

type comicInfoKey struct {
	bookID, version string
}

type comicInfoJob struct {
	book, logical database.Book
	hint          comicInfoHint
	password      string
	scope         database.UserScope
	scoped        bool
}

type comicInfoCache struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	queue      chan comicInfoJob
	mu         sync.Mutex
	pending    map[comicInfoKey]struct{}
	extracting map[string]chan struct{}
	parser     ComicInfoParser
}

type comicInfoZIPReader struct{ io.ReaderAt }

type comicInfoHintKey struct{}
type suppressComicInfoKey struct{}

// WithoutComicInfoCaching is used for index-only work and speculative reads.
func WithoutComicInfoCaching(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressComicInfoKey{}, true)
}

func comicInfoEntry(name string) bool {
	return strings.EqualFold(path.Base(strings.ReplaceAll(name, "\\", "/")), "ComicInfo.xml")
}

func withComicInfoHint(ctx context.Context, hint *comicInfoHint) context.Context {
	if ctx.Value(suppressComicInfoKey{}) == true {
		return ctx
	}
	return context.WithValue(ctx, comicInfoHintKey{}, hint)
}

func (s *Service) startComicInfoCache() {
	ctx, cancel := context.WithCancel(context.Background())
	c := &comicInfoCache{
		ctx: ctx, cancel: cancel, done: make(chan struct{}), queue: make(chan comicInfoJob, 32),
		pending: make(map[comicInfoKey]struct{}), extracting: make(map[string]chan struct{}),
	}
	s.comicInfo = c
	go func() {
		defer close(c.done)
		for c.ctx.Err() == nil {
			select {
			case <-c.ctx.Done():
				return
			case job := <-c.queue:
				ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
				ctx = withLogicalBookContext(ctx, job.logical)
				if job.scoped {
					ctx = database.WithUserScope(ctx, job.scope)
				}
				if job.password != "" {
					ctx = WithExplicitArchivePassword(ctx, job.password)
				}
				_, _, _ = s.extractPhysicalComicInfo(ctx, job, false)
				cancel()
				c.mu.Lock()
				delete(c.pending, comicInfoKey{job.book.ID, BookVersion(job.book)})
				c.mu.Unlock()
			}
		}
	}()
}

func (s *Service) SetComicInfoParser(parser ComicInfoParser) {
	s.comicInfo.mu.Lock()
	defer s.comicInfo.mu.Unlock()
	s.comicInfo.parser = parser
}

func (s *Service) queueComicInfo(ctx context.Context, book, logical database.Book, hint comicInfoHint, readErr error) {
	c := s.comicInfo
	if c == nil || c.ctx.Err() != nil || ctx.Value(suppressComicInfoKey{}) == true {
		return
	}
	switch {
	case errors.Is(readErr, ErrSolidRAR):
		hint.status = ComicInfoSkippedSolid
	case isArchivePasswordError(readErr):
		hint.status = ComicInfoSkippedEncrypted
	case readErr != nil:
		return
	}
	password, _ := explicitArchivePasswordFromContext(ctx)
	scope, scoped := database.UserScopeFromContext(ctx)
	key := comicInfoKey{book.ID, BookVersion(book)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.pending[key]; exists || c.parser == nil {
		return
	}
	select {
	case c.queue <- comicInfoJob{book: book, logical: logical, hint: hint, password: password, scope: scope, scoped: scoped}:
		c.pending[key] = struct{}{}
	default:
	}
}

func comicInfoTerminal(status string) bool {
	return status == ComicInfoParsed || status == ComicInfoAbsent || status == ComicInfoSkippedSolid ||
		status == ComicInfoSkippedUnsupported
}

// ExtractBookComicInfo reads the embedded metadata for an explicitly selected book.
func (s *Service) ExtractBookComicInfo(ctx context.Context, book database.Book) (string, error) {
	status, _, err := s.ExtractBookComicInfoWithOptions(ctx, book, false)
	return status, err
}

// ExtractBookComicInfoWithOptions also reports whether metadata was newly parsed.
func (s *Service) ExtractBookComicInfoWithOptions(ctx context.Context, book database.Book, force bool) (string, bool, error) {
	ctx = withLogicalBookContext(ctx, book)
	candidates, _ := s.resolveCandidates(ctx, book)
	for i, physical := range candidates {
		if physical.LibraryID != book.LibraryID && !s.store.IsLibraryEnabled(ctx, physical.LibraryID) {
			continue
		}
		status, changed, err := s.extractPhysicalComicInfo(ctx, comicInfoJob{book: physical, logical: book}, force)
		if i == len(candidates)-1 || !shouldFallbackReplica(err) {
			return status, changed, err
		}
	}
	return "", false, sql.ErrNoRows
}

func (c *comicInfoCache) claim(ctx context.Context, id string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if prev := c.extracting[id]; prev != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-prev:
			}
			continue
		}
		done := make(chan struct{})
		c.extracting[id] = done
		c.mu.Unlock()
		return func() {
			c.mu.Lock()
			delete(c.extracting, id)
			close(done)
			c.mu.Unlock()
		}, nil
	}
}

func (s *Service) extractPhysicalComicInfo(ctx context.Context, job comicInfoJob, force bool) (string, bool, error) {
	book := job.book
	release, err := s.comicInfo.claim(ctx, book.ID)
	if err != nil {
		return "", false, err
	}
	defer release()
	// Recheck the catalog before opening a queued book or committing to its version.
	checkVersion := func() error {
		if _, err := s.store.BookByID(ctx, job.logical.ID); err != nil {
			return err
		}
		if book.LibraryID != job.logical.LibraryID && !s.store.IsLibraryEnabled(ctx, book.LibraryID) {
			return sql.ErrNoRows
		}
		current, err := s.store.BookByIDAny(ctx, book.ID)
		if err == nil && BookVersion(current) != BookVersion(book) {
			return sql.ErrNoRows
		}
		return err
	}
	if err := checkVersion(); err != nil {
		return "", false, err
	}
	version := BookVersion(book)
	current, _, err := s.store.BookMetadataByID(ctx, book.ID)
	if err != nil {
		return "", false, err
	}
	if !force && current.ComicInfoVersion == version && comicInfoTerminal(current.ComicInfoStatus) {
		if job.logical.ID != "" && job.logical.ID != book.ID {
			return s.syncLogicalBookFromPhysical(ctx, job.logical.ID, version, current)
		}
		return current.ComicInfoStatus, false, nil
	}
	var raw []byte
	status := job.hint.status
	if status == "" {
		if job.hint.format != "" && job.hint.name == "" {
			status = ComicInfoAbsent
		} else {
			raw, err = s.readComicInfo(ctx, book, job.hint)
			switch {
			case errors.Is(err, ErrSolidRAR):
				status = ComicInfoSkippedSolid
			case isArchivePasswordError(err):
				status = ComicInfoSkippedEncrypted
			case errors.Is(err, ErrUnsupportedArchive), errors.Is(err, ErrMultiVolumeRAR):
				status = ComicInfoSkippedUnsupported
			case err != nil:
				return "", false, err
			case raw == nil:
				status = ComicInfoAbsent
			}
		}
	}
	if err := checkVersion(); err != nil {
		return "", false, err
	}
	if status != "" {
		if current.ComicInfoVersion != version || current.ComicInfoStatus != status {
			if err := s.store.SetBookComicInfoSentinel(ctx, book.ID, version, status); err != nil {
				return status, false, err
			}
		}
		if job.logical.ID != "" && job.logical.ID != book.ID {
			if err := s.syncLogicalBookSentinel(ctx, job.logical.ID, version, status); err != nil {
				return status, false, err
			}
		}
		return status, false, nil
	}
	if err := s.persistComicInfo(ctx, job, version, raw, force); err != nil {
		return "", false, err
	}
	return ComicInfoParsed, true, nil
}

func (s *Service) readComicInfo(ctx context.Context, book database.Book, hint comicInfoHint) ([]byte, error) {
	format := hint.format
	if format == "" {
		format = s.effectiveFormat(ctx, book)
	}
	read := func(format string) ([]byte, error) {
		switch format {
		case formatZIP:
			return s.zip.readComicInfo(ctx, book, hint)
		case formatRAR:
			return s.rar.readComicInfo(ctx, book, hint)
		default:
			return nil, ErrUnsupportedArchive
		}
	}
	raw, err := read(format)
	if hint.format == "" && (errors.Is(err, ErrInvalidZIP) || errors.Is(err, ErrInvalidRAR)) {
		if detected := s.detectMagicFormat(ctx, book); detected != "" && detected != format {
			return read(detected)
		}
	}
	return raw, err
}

func (z *ZIPService) readComicInfo(ctx context.Context, book database.Book, hint comicInfoHint) ([]byte, error) {
	blockSize := z.store.Int64Setting(ctx, "range_block_size", 1<<20)
	readerAt, size, err := openBookReaderAtWithSize(ctx, book, z.store, z.client, z.webdav, z.cache, z.logger, blockSize)
	if err != nil {
		return nil, err
	}
	defer readerAt.Close()
	entry := hint.zipEntry
	var encrypted bool
	if entry != nil {
		hint.zipReader.ReaderAt = readerAt
		defer func() { hint.zipReader.ReaderAt = nil }()
		encrypted = entry.Flags&1 != 0
	} else {
		reader, err := zip.NewReader(readerAt, size)
		if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
			return nil, ErrInvalidZIP
		}
		for _, file := range reader.File {
			encrypted = encrypted || file.Flags&1 != 0
			if entry == nil && !file.FileInfo().IsDir() && comicInfoEntry(file.Name) {
				entry = file
			}
		}
	}
	var candidates []database.ArchivePasswordCandidate
	if encrypted {
		candidates, _, err = resolveArchivePasswordCandidates(ctx, z.store, book)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, ErrArchivePasswordRequired
		}
	}
	if entry == nil {
		return nil, nil
	}
	if entry.UncompressedSize64 > maxComicInfoSize || entry.CompressedSize64 > 2*maxComicInfoSize {
		return nil, ErrPageTooLarge
	}
	encrypted, scheme, method, check, ae, err := inspectZIPEntryCrypto(entry)
	if err != nil {
		return nil, err
	}
	offset, err := entry.DataOffset()
	if err != nil {
		return nil, err
	}
	item := PageEntry{
		UncompressedSize: entry.UncompressedSize64, Method: method, CRC32: entry.CRC32,
		Encrypted: encrypted, EncScheme: scheme, EncCheckByte: check, EncAEVersion: ae,
	}
	compressed := make([]byte, int(entry.CompressedSize64))
	if _, err := readerAt.ReadAt(compressed, offset); err != nil {
		return nil, err
	}
	if !encrypted {
		return decodeZIPEntryBytes(compressed, item, maxComicInfoSize, "")
	}
	for _, candidate := range candidates {
		data, err := decodeZIPEntryBytes(compressed, item, maxComicInfoSize, candidate.Password)
		if !isArchivePasswordError(err) {
			return data, err
		}
	}
	return nil, ErrArchivePasswordInvalid
}

func rarComicInfoEntry(files []*rardecode.File, name string) (PageEntry, error) {
	var entry PageEntry
	for i, file := range files {
		if err := validateRARFile(file); err != nil {
			return PageEntry{}, err
		}
		if entry.Name == "" && !file.IsDir && comicInfoEntry(file.Name) && (name == "" || file.Name == name) {
			entry = PageEntry{
				Name: file.Name, ArchiveIndex: i, UncompressedSize: uint64(file.UnPackedSize),
				CompressedSize: uint64(file.PackedSize), Encrypted: file.Encrypted || file.HeaderEncrypted,
			}
		}
	}
	return entry, nil
}

func (r *RARService) readComicInfo(ctx context.Context, book database.Book, hint comicInfoHint) ([]byte, error) {
	if file := hint.rarEntry; file != nil {
		entry := PageEntry{
			Name: file.Name, UncompressedSize: uint64(file.UnPackedSize),
			Encrypted: file.Encrypted || file.HeaderEncrypted,
		}
		if entry.UncompressedSize > maxComicInfoSize {
			return nil, ErrPageTooLarge
		}
		hint.rarFS.ctx = ctx
		data, err := readRARFile(hint.rarFS, file, entry, maxComicInfoSize, hint.rarPassword)
		if !isArchivePasswordError(err) || !hint.rarPassword {
			return data, err
		}
	}
	candidates, explicit, err := resolveArchivePasswordCandidates(ctx, r.store, book)
	if err != nil {
		return nil, err
	}
	if !explicit {
		candidates = append(candidates, database.ArchivePasswordCandidate{})
	}
	for _, candidate := range candidates {
		files, remote, err := r.listFilesWithPassword(ctx, book, candidate.Password)
		if isArchivePasswordError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entry, err := rarComicInfoEntry(files, hint.name)
		if err != nil {
			return nil, err
		}
		if candidate.Password == "" {
			for _, file := range files {
				if file.Encrypted || file.HeaderEncrypted {
					return nil, ErrArchivePasswordRequired
				}
			}
		}
		if entry.Name == "" {
			return nil, nil
		}
		if entry.UncompressedSize > maxComicInfoSize {
			return nil, ErrPageTooLarge
		}
		data, err := readRARFileEntry(remote, files, entry, maxComicInfoSize, candidate.Password != "")
		if !isArchivePasswordError(err) {
			return data, err
		}
	}
	return nil, ErrArchivePasswordInvalid
}

func (s *Service) syncLogicalBookFromPhysical(ctx context.Context, logicalID, version string, physical database.BookMetadata) (string, bool, error) {
	logicalMeta, _, err := s.store.BookMetadataByID(ctx, logicalID)
	if err != nil {
		return "", false, err
	}
	if logicalMeta.ComicInfoVersion == version && comicInfoTerminal(logicalMeta.ComicInfoStatus) &&
		logicalMeta.ComicInfoStatus == physical.ComicInfoStatus {
		return physical.ComicInfoStatus, false, nil
	}
	if physical.ComicInfoStatus != ComicInfoParsed {
		return physical.ComicInfoStatus, false, s.store.SetBookComicInfoSentinel(ctx, logicalID, version, physical.ComicInfoStatus)
	}
	meta := database.BookScrapedMetadata{
		Title:            physical.Title,
		Summary:          physical.Summary,
		Number:           physical.Number,
		NumberSort:       physical.NumberSort,
		ReleaseDate:      physical.ReleaseDate,
		ISBN:             physical.ISBN,
		SeriesGroup:      physical.SeriesGroup,
		StoryArc:         physical.StoryArc,
		StoryArcNumber:   physical.StoryArcNumber,
		ComicInfoVersion: version,
		ComicInfoStatus:  physical.ComicInfoStatus,
	}
	if len(physical.Authors) > 0 {
		meta.Authors = slices.Clone(physical.Authors)
	}
	if len(physical.Tags) > 0 {
		meta.Tags = slices.Clone(physical.Tags)
	}
	if len(physical.Links) > 0 {
		meta.Links = slices.Clone(physical.Links)
	}
	updated, err := s.syncLogicalBookMetadata(ctx, logicalID, version, meta)
	return physical.ComicInfoStatus, updated, err
}

func (s *Service) syncLogicalBookMetadata(ctx context.Context, logicalID, version string, meta database.BookScrapedMetadata) (bool, error) {
	err := s.store.BackfillBookComicInfo(ctx, logicalID, meta, false)
	if errors.Is(err, database.ErrMetadataChanged) {
		return false, s.syncLogicalBookSentinel(ctx, logicalID, version, meta.ComicInfoStatus)
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) syncLogicalBookSentinel(ctx context.Context, logicalID, version, status string) error {
	logicalMeta, _, err := s.store.BookMetadataByID(ctx, logicalID)
	if err != nil {
		return err
	}
	if logicalMeta.ComicInfoVersion == version && logicalMeta.ComicInfoStatus == status {
		return nil
	}
	return s.store.SetBookComicInfoSentinel(ctx, logicalID, version, status)
}

func (s *Service) persistComicInfo(ctx context.Context, job comicInfoJob, version string, raw []byte, force bool) error {
	s.comicInfo.mu.Lock()
	parser := s.comicInfo.parser
	s.comicInfo.mu.Unlock()
	if parser == nil {
		return errors.New("ComicInfo parser is not configured")
	}
	meta, series, err := parser(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := s.store.BackfillSeriesMetadataFromComicInfo(ctx, job.book.SeriesID, series); err != nil {
		return err
	}
	if job.logical.SeriesID != "" && job.logical.SeriesID != job.book.SeriesID {
		if err := s.store.BackfillSeriesMetadataFromComicInfo(ctx, job.logical.SeriesID, series); err != nil {
			return err
		}
	}
	meta.ComicInfoVersion, meta.ComicInfoStatus = version, ComicInfoParsed
	if err := s.store.BackfillBookComicInfo(ctx, job.book.ID, meta, force); err != nil {
		return err
	}
	if job.logical.ID != "" && job.logical.ID != job.book.ID {
		if _, err := s.syncLogicalBookMetadata(ctx, job.logical.ID, version, meta); err != nil {
			return err
		}
	}
	return nil
}
