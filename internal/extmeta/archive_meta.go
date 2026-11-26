package extmeta

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const maxEmbeddedMetaBytes int64 = 512 << 10 // 512 KiB

var (
	galleryPathIDRe = regexp.MustCompile(`(?i)/g/(\d+)/[0-9a-f]+`)
	bgmSubjectIDRe  = regexp.MustCompile(`(?i)/subject/(\d+)`)
)

// EmbeddedLink represents a validated HTTP/HTTPS web reference inside embedded metadata.
type EmbeddedLink struct {
	Label string `json:"label,omitempty"`
	URL   string `json:"url"`
}

// EmbeddedAuthor represents a role-qualified creator inside embedded metadata.
type EmbeddedAuthor struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// EmbeddedMeta holds metadata parsed directly from archive-embedded ComicInfo.xml
// or galleryinfo.txt sidecar files.
type EmbeddedMeta struct {
	Title            string           `json:"title,omitempty"`
	Series           string           `json:"series,omitempty"`
	SeriesSort       string           `json:"seriesSort,omitempty"`
	LocalizedSeries  string           `json:"localizedSeries,omitempty"`
	Number           string           `json:"number,omitempty"`
	NumberSort       *float64         `json:"numberSort,omitempty"`
	Volume           string           `json:"volume,omitempty"`
	Count            *int             `json:"count,omitempty"`
	Authors          []string         `json:"authors,omitempty"`
	DetailedAuthors  []EmbeddedAuthor `json:"detailedAuthors,omitempty"`
	Summary          string           `json:"summary,omitempty"`
	ReleaseDate      string           `json:"releaseDate,omitempty"`
	Publisher        string           `json:"publisher,omitempty"`
	LanguageISO      string           `json:"languageIso,omitempty"`
	ReadingDirection string           `json:"readingDirection,omitempty"`
	AgeRating        string           `json:"ageRating,omitempty"`
	AgeRatingInt     *int             `json:"ageRatingInt,omitempty"`
	Tags             []string         `json:"tags,omitempty"`
	Genre            []string         `json:"genre,omitempty"`
	Genres           []string         `json:"genres,omitempty"`
	Links            []EmbeddedLink   `json:"links,omitempty"`
	ExternalURL      string           `json:"externalUrl,omitempty"`
	ExternalURLs     []string         `json:"externalUrls,omitempty"`
	ExternalID       string           `json:"externalId,omitempty"`
	ISBN             string           `json:"isbn,omitempty"`
	SeriesGroup      string           `json:"seriesGroup,omitempty"`
	StoryArc         string           `json:"storyArc,omitempty"`
	StoryArcNumber   string           `json:"storyArcNumber,omitempty"`
	IsNSFW           bool             `json:"isNsfw,omitempty"`
}

type comicInfoXMLDoc struct {
	XMLName          xml.Name `xml:"ComicInfo"`
	Title            string   `xml:"Title"`
	Series           string   `xml:"Series"`
	SeriesSort       string   `xml:"SeriesSort"`
	LocalizedSeries  string   `xml:"LocalizedSeries"`
	Number           string   `xml:"Number"`
	Volume           string   `xml:"Volume"`
	Count            string   `xml:"Count"`
	Summary          string   `xml:"Summary"`
	Year             string   `xml:"Year"`
	Month            string   `xml:"Month"`
	Day              string   `xml:"Day"`
	Publisher        string   `xml:"Publisher"`
	LanguageISO      string   `xml:"LanguageISO"`
	Manga            string   `xml:"Manga"`
	ReadingDirection string   `xml:"ReadingDirection"`
	Writer           string   `xml:"Writer"`
	Penciller        string   `xml:"Penciller"`
	Inker            string   `xml:"Inker"`
	Colorist         string   `xml:"Colorist"`
	Letterer         string   `xml:"Letterer"`
	CoverArtist      string   `xml:"CoverArtist"`
	Editor           string   `xml:"Editor"`
	Translator       string   `xml:"Translator"`
	Genre            []string `xml:"Genre"`
	Genres           []string `xml:"Genres"`
	Tags             []string `xml:"Tags"`
	Web              []string `xml:"Web"`
	AgeRating        string   `xml:"AgeRating"`
	GTIN             string   `xml:"GTIN"`
	ISBN             string   `xml:"ISBN"`
	SeriesGroup      string   `xml:"SeriesGroup"`
	StoryArc         string   `xml:"StoryArc"`
	StoryArcNumber   string   `xml:"StoryArcNumber"`
}

// ParseComicInfoXML parses a ComicInfo.xml stream up to 512 KiB using standard encoding/xml.
func ParseComicInfoXML(r io.Reader) (*EmbeddedMeta, error) {
	if r == nil {
		return nil, errors.New("ComicInfo.xml reader must not be nil")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxEmbeddedMetaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read ComicInfo.xml: %w", err)
	}
	if int64(len(raw)) > maxEmbeddedMetaBytes {
		return nil, fmt.Errorf("ComicInfo.xml exceeds %d bytes limit", maxEmbeddedMetaBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty ComicInfo.xml")
	}

	var doc comicInfoXMLDoc
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode ComicInfo.xml: %w", err)
	}

	roleFields := []struct {
		role  string
		value string
	}{
		{"writer", doc.Writer},
		{"penciller", doc.Penciller},
		{"inker", doc.Inker},
		{"colorist", doc.Colorist},
		{"letterer", doc.Letterer},
		{"cover", doc.CoverArtist},
		{"editor", doc.Editor},
		{"translator", doc.Translator},
	}

	var (
		rawAuthors      []string
		detailedAuthors []EmbeddedAuthor
		seenDetailed    = make(map[string]struct{})
	)
	for _, rf := range roleFields {
		for _, name := range splitMetaList(rf.value) {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				continue
			}
			rawAuthors = append(rawAuthors, trimmed)
			key := strings.ToLower(trimmed) + "\x00" + rf.role
			if _, dup := seenDetailed[key]; !dup {
				seenDetailed[key] = struct{}{}
				detailedAuthors = append(detailedAuthors, EmbeddedAuthor{
					Name: trimmed,
					Role: rf.role,
				})
			}
		}
	}

	var rawGenres []string
	for _, g := range append(append([]string{}, doc.Genre...), doc.Genres...) {
		rawGenres = append(rawGenres, splitMetaList(g)...)
	}
	genres := dedupeStrings(rawGenres)

	var rawTags []string
	for _, t := range doc.Tags {
		rawTags = append(rawTags, splitMetaList(t)...)
	}
	tags := dedupeStrings(rawTags)

	links, extURLs, extURL, extID := parseComicInfoWebLinks(doc.Web)
	ageRatingStr := strings.TrimSpace(doc.AgeRating)
	ageRatingInt, isNSFW := parseComicInfoAgeRating(ageRatingStr)

	number := cleanSentinels(doc.Number)
	volume := cleanSentinels(doc.Volume)
	numberSort := parseComicInfoNumberSort(number)
	count := parseComicInfoCount(doc.Count)
	releaseDate := formatComicInfoReleaseDate(doc.Year, doc.Month, doc.Day)
	readDir := mapComicInfoReadingDirection(doc.Manga, doc.ReadingDirection)

	isbn := strings.TrimSpace(doc.GTIN)
	if isbn == "" {
		isbn = strings.TrimSpace(doc.ISBN)
	}

	return &EmbeddedMeta{
		Title:            strings.TrimSpace(doc.Title),
		Series:           strings.TrimSpace(doc.Series),
		SeriesSort:       strings.TrimSpace(doc.SeriesSort),
		LocalizedSeries:  strings.TrimSpace(doc.LocalizedSeries),
		Number:           number,
		NumberSort:       numberSort,
		Volume:           volume,
		Count:            count,
		Authors:          dedupeStrings(rawAuthors),
		DetailedAuthors:  detailedAuthors,
		Summary:          strings.TrimSpace(doc.Summary),
		ReleaseDate:      releaseDate,
		Publisher:        strings.TrimSpace(doc.Publisher),
		LanguageISO:      strings.TrimSpace(doc.LanguageISO),
		ReadingDirection: readDir,
		AgeRating:        ageRatingStr,
		AgeRatingInt:     ageRatingInt,
		Tags:             tags,
		Genre:            genres,
		Genres:           genres,
		Links:            links,
		ExternalURL:      extURL,
		ExternalURLs:     extURLs,
		ExternalID:       extID,
		ISBN:             isbn,
		SeriesGroup:      strings.TrimSpace(doc.SeriesGroup),
		StoryArc:         strings.TrimSpace(doc.StoryArc),
		StoryArcNumber:   strings.TrimSpace(doc.StoryArcNumber),
		IsNSFW:           isNSFW,
	}, nil
}

// ParseGalleryInfoTXT parses a galleryinfo.txt metadata stream up to 512 KiB using bufio.
func ParseGalleryInfoTXT(r io.Reader) (*EmbeddedMeta, error) {
	if r == nil {
		return nil, errors.New("galleryinfo.txt reader must not be nil")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxEmbeddedMetaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read galleryinfo.txt: %w", err)
	}
	if int64(len(raw)) > maxEmbeddedMetaBytes {
		return nil, fmt.Errorf("galleryinfo.txt exceeds %d bytes limit", maxEmbeddedMetaBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty galleryinfo.txt")
	}

	meta := &EmbeddedMeta{}
	var (
		rawTags       []string
		directAuthors []string
		category      string
		jpnTitle      string
		inComment     bool
		commentLines  []string
	)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 16<<10), int(maxEmbeddedMetaBytes))

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if inComment {
			commentLines = append(commentLines, line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+1:])

		switch key {
		case "title":
			if meta.Title == "" {
				meta.Title = val
			}
		case "japanese title", "title_jpn", "original title":
			if jpnTitle == "" {
				jpnTitle = val
			}
		case "series":
			if meta.Series == "" {
				meta.Series = val
			}
		case "artist", "author", "authors", "group", "circle":
			directAuthors = append(directAuthors, splitMetaList(val)...)
		case "tags", "tag":
			rawTags = append(rawTags, splitMetaList(val)...)
		case "category":
			category = val
		case "url", "source", "source url", "gallery url":
			if safeURL := cleanSafeExternalURL(val); safeURL != "" {
				meta.ExternalURL = safeURL
				meta.ExternalURLs = []string{safeURL}
				meta.Links = []EmbeddedLink{{Label: inferLinkLabel(safeURL), URL: safeURL}}
				if meta.ExternalID == "" {
					meta.ExternalID = extractExternalIDFromURL(safeURL)
				}
			}
		case "gid", "id", "gallery id":
			if meta.ExternalID == "" && val != "" {
				meta.ExternalID = val
			}
		case "uploader comment", "comment", "summary", "description":
			inComment = true
			if val != "" {
				commentLines = append(commentLines, val)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan galleryinfo.txt: %w", err)
	}

	if meta.Title == "" && jpnTitle != "" {
		meta.Title = jpnTitle
	}
	if meta.Series == "" && meta.Title != "" {
		meta.Series = meta.Title
	}

	parsedAuthors, parsedTags := ParseGalleryTags(rawTags)
	meta.Authors = dedupeStrings(append(directAuthors, parsedAuthors...))
	meta.Tags = parsedTags

	if category != "" {
		meta.Genre = []string{category}
		meta.Genres = []string{category}
		meta.IsNSFW = IsGalleryCategoryNSFW(category)
	}
	if len(commentLines) > 0 {
		meta.Summary = strings.TrimSpace(strings.Join(commentLines, "\n"))
	}

	return meta, nil
}

func splitMetaList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';'
	})
}

func cleanSentinels(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "-1" {
		return ""
	}
	return trimmed
}

func parseComicInfoNumberSort(raw string) *float64 {
	trimmed := strings.TrimSpace(strings.TrimPrefix(raw, "#"))
	if trimmed == "" || trimmed == "-1" {
		return nil
	}
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func parseComicInfoCount(raw string) *int {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

func formatComicInfoReleaseDate(yearRaw, monthRaw, dayRaw string) string {
	year, err := strconv.Atoi(strings.TrimSpace(yearRaw))
	if err != nil || year < 1 || year > 9999 {
		return ""
	}
	month, mErr := strconv.Atoi(strings.TrimSpace(monthRaw))
	if mErr != nil || month < 1 || month > 12 {
		return fmt.Sprintf("%04d", year)
	}
	day, dErr := strconv.Atoi(strings.TrimSpace(dayRaw))
	if dErr != nil || day < 1 || day > 31 {
		return fmt.Sprintf("%04d-%02d", year, month)
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

func mapComicInfoReadingDirection(mangaRaw, dirRaw string) string {
	val := strings.TrimSpace(mangaRaw)
	if val == "" {
		val = strings.TrimSpace(dirRaw)
	}
	switch strings.ToLower(val) {
	case "yesandrighttoleft", "yes", "righttoleft", "right_to_left", "rtl":
		return "RIGHT_TO_LEFT"
	case "no", "lefttoright", "left_to_right", "ltr":
		return "LEFT_TO_RIGHT"
	case "vertical":
		return "VERTICAL"
	case "webtoon":
		return "WEBTOON"
	default:
		return ""
	}
}

func parseComicInfoAgeRating(rating string) (*int, bool) {
	trimmed := strings.TrimSpace(rating)
	if trimmed == "" {
		return nil, false
	}
	intPtr := func(v int) *int { return &v }
	lower := strings.ToLower(trimmed)
	switch lower {
	case "unknown", "rating pending":
		return nil, false
	case "everyone", "g", "all ages", "general":
		return intPtr(0), false
	case "early childhood":
		return intPtr(3), false
	case "kids to adults":
		return intPtr(6), false
	case "pg":
		return intPtr(8), false
	case "everyone 10+":
		return intPtr(10), false
	case "teen", "teen 13+", "t":
		return intPtr(13), false
	case "ma15+", "ma 15+", "r15", "r15+", "15+":
		return intPtr(15), false
	case "16+":
		return intPtr(16), false
	case "mature 17+", "m", "17+":
		return intPtr(17), false
	case "adults only 18+", "r18+", "r18", "r-18", "x18+", "x18", "18+", "x", "explicit":
		return intPtr(18), true
	default:
		if n, err := strconv.Atoi(strings.TrimSuffix(lower, "+")); err == nil && n >= 0 && n <= 100 {
			return intPtr(n), n >= 18
		}
		return nil, false
	}
}

func isComicInfoAgeRatingNSFW(rating string) bool {
	_, nsfw := parseComicInfoAgeRating(rating)
	return nsfw
}

func parseComicInfoWebLinks(rawValues []string) (links []EmbeddedLink, urls []string, firstURL string, extID string) {
	seen := make(map[string]struct{})
	for _, block := range rawValues {
		tokens := strings.FieldsFunc(block, func(r rune) bool {
			return unicode.IsSpace(r) || r == ',' || r == ';'
		})
		for _, tok := range tokens {
			safe := cleanSafeExternalURL(tok)
			if safe == "" {
				continue
			}
			if _, dup := seen[safe]; dup {
				continue
			}
			seen[safe] = struct{}{}
			urls = append(urls, safe)
			links = append(links, EmbeddedLink{
				Label: inferLinkLabel(safe),
				URL:   safe,
			})
			if firstURL == "" {
				firstURL = safe
			}
			if extID == "" {
				extID = extractExternalIDFromURL(safe)
			}
		}
	}
	return links, urls, firstURL, extID
}

func inferLinkLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "Link"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "bgm.tv" || host == "bangumi.tv" || strings.HasSuffix(host, ".bgm.tv") || strings.HasSuffix(host, ".bangumi.tv"):
		return "Bangumi"
	case host == "anilist.co" || strings.HasSuffix(host, ".anilist.co"):
		return "AniList"
	case host == "myanimelist.net" || strings.HasSuffix(host, ".myanimelist.net"):
		return "MyAnimeList"
	case host == "mangadex.org" || strings.HasSuffix(host, ".mangadex.org"):
		return "MangaDex"
	case host == "mangaupdates.com" || strings.HasSuffix(host, ".mangaupdates.com"):
		return "MangaUpdates"
	case host == "comicvine.gamespot.com":
		return "ComicVine"
	case host != "":
		return host
	default:
		return "Link"
	}
}

func cleanSafeExternalURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.String()
	default:
		return ""
	}
}

func extractExternalIDFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if m := galleryPathIDRe.FindStringSubmatch(rawURL); len(m) == 2 {
		return m[1]
	}
	if m := bgmSubjectIDRe.FindStringSubmatch(rawURL); len(m) == 2 {
		return m[1]
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	trimmedPath := strings.Trim(u.Path, "/")
	if trimmedPath == "" {
		return ""
	}
	parts := strings.Split(trimmedPath, "/")
	last := parts[len(parts)-1]
	if _, err := strconv.ParseInt(last, 10, 64); err == nil {
		return last
	}
	return ""
}
