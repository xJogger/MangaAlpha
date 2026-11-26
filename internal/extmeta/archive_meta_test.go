package extmeta

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseComicInfoXMLFullComicInfo21AndKavitaExtensions(t *testing.T) {
	t.Parallel()

	xmlPayload := "\xEF\xBB\xBF" + `<?xml version="1.0" encoding="utf-8"?>
<ComicInfo xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <Title>Frieren Vol. 03</Title>
  <Series>葬送的芙莉莲</Series>
  <SeriesSort>Frieren, Sousou no</SeriesSort>
  <LocalizedSeries>Sousou no Frieren</LocalizedSeries>
  <Number>03.5</Number>
  <Volume>3</Volume>
  <Count>13</Count>
  <Summary>Journey to Ende.</Summary>
  <Year>2020</Year>
  <Month>12</Month>
  <Day>18</Day>
  <Publisher>小学馆</Publisher>
  <LanguageISO>zh</LanguageISO>
  <Manga>YesAndRightToLeft</Manga>
  <Writer>山田钟人</Writer>
  <Penciller>阿部司</Penciller>
  <Inker>阿部司, 助手A</Inker>
  <Colorist>上色组</Colorist>
  <Letterer>嵌字组</Letterer>
  <CoverArtist>阿部司</CoverArtist>
  <Editor>编辑B</Editor>
  <Translator>译者C</Translator>
  <Genre>Fantasy, Adventure, Fantasy</Genre>
  <Tags>magic, elf, magic</Tags>
  <Web>
    https://bgm.tv/subject/305025
    https://anilist.co/manga/118586/, javascript:alert(1)
    https://myanimelist.net/manga/126287
  </Web>
  <AgeRating>Teen</AgeRating>
  <GTIN>978-4098502851</GTIN>
  <SeriesGroup>Sunday Comics</SeriesGroup>
  <StoryArc>First-Class Mage Exam</StoryArc>
  <StoryArcNumber>1</StoryArcNumber>
</ComicInfo>`

	meta, err := ParseComicInfoXML(strings.NewReader(xmlPayload))
	if err != nil {
		t.Fatalf("ParseComicInfoXML failed: %v", err)
	}

	if meta.Title != "Frieren Vol. 03" || meta.Series != "葬送的芙莉莲" ||
		meta.SeriesSort != "Frieren, Sousou no" || meta.LocalizedSeries != "Sousou no Frieren" {
		t.Fatalf("unexpected series/title fields: %+v", meta)
	}
	if meta.Number != "03.5" || meta.NumberSort == nil || *meta.NumberSort != 3.5 {
		t.Fatalf("unexpected Number/NumberSort: number=%q sort=%v", meta.Number, meta.NumberSort)
	}
	if meta.Volume != "3" || meta.Count == nil || *meta.Count != 13 {
		t.Fatalf("unexpected Volume/Count: vol=%q count=%v", meta.Volume, meta.Count)
	}
	if meta.ReleaseDate != "2020-12-18" {
		t.Fatalf("ReleaseDate = %q, want 2020-12-18", meta.ReleaseDate)
	}
	if meta.Publisher != "小学馆" || meta.LanguageISO != "zh" || meta.ReadingDirection != "RIGHT_TO_LEFT" {
		t.Fatalf("unexpected publisher/lang/dir: %+v", meta)
	}
	if meta.AgeRating != "Teen" || meta.AgeRatingInt == nil || *meta.AgeRatingInt != 13 || meta.IsNSFW {
		t.Fatalf("unexpected Teen age rating: %+v", meta)
	}
	if meta.ISBN != "978-4098502851" {
		t.Fatalf("ISBN = %q, want 978-4098502851", meta.ISBN)
	}
	if meta.SeriesGroup != "Sunday Comics" || meta.StoryArc != "First-Class Mage Exam" || meta.StoryArcNumber != "1" {
		t.Fatalf("unexpected arc/group fields: %+v", meta)
	}
	if len(meta.Genre) != 2 || len(meta.Genres) != 2 || len(meta.Tags) != 2 {
		t.Fatalf("unexpected genres/tags: genre=%v genres=%v tags=%v", meta.Genre, meta.Genres, meta.Tags)
	}
	if len(meta.ExternalURLs) != 3 || len(meta.Links) != 3 || meta.ExternalURL != "https://bgm.tv/subject/305025" || meta.ExternalID != "305025" {
		t.Fatalf("unexpected links/urls: links=%+v urls=%v extURL=%q extID=%q", meta.Links, meta.ExternalURLs, meta.ExternalURL, meta.ExternalID)
	}
	if meta.Links[0].Label != "Bangumi" || meta.Links[1].Label != "AniList" || meta.Links[2].Label != "MyAnimeList" {
		t.Fatalf("unexpected link labels: %+v", meta.Links)
	}

	// Verify all 8 Komga author roles + deduplicated legacy Authors slice.
	wantRoles := map[string]string{
		"writer":     "山田钟人",
		"penciller":  "阿部司",
		"colorist":   "上色组",
		"letterer":   "嵌字组",
		"cover":      "阿部司",
		"editor":     "编辑B",
		"translator": "译者C",
	}
	if len(meta.DetailedAuthors) != 9 {
		t.Fatalf("expected 9 detailed author entries (including 阿部司 as penciller/inker/cover + 助手A), got %+v", meta.DetailedAuthors)
	}
	for role, wantName := range wantRoles {
		found := false
		for _, da := range meta.DetailedAuthors {
			if da.Role == role && da.Name == wantName {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing DetailedAuthors role %q -> %q in %+v", role, wantName, meta.DetailedAuthors)
		}
	}
	if len(meta.Authors) != 7 {
		t.Fatalf("expected 7 deduplicated author names, got %+v", meta.Authors)
	}
}

func TestParseComicInfoXMLAgeRatingsDatesDirectionsAndSafetyLimits(t *testing.T) {
	t.Parallel()

	ratingCases := []struct {
		raw      string
		wantInt  *int
		wantNSFW bool
	}{
		{"X18+", intPtr(18), true},
		{"Adults Only 18+", intPtr(18), true},
		{"R18+", intPtr(18), true},
		{"R18", intPtr(18), true},
		{"18+", intPtr(18), true},
		{"X", intPtr(18), true},
		{"Explicit", intPtr(18), true},
		{"Mature 17+", intPtr(17), false},
		{"M", intPtr(17), false},
		{"MA15+", intPtr(15), false},
		{"Teen", intPtr(13), false},
		{"Everyone 10+", intPtr(10), false},
		{"PG", intPtr(8), false},
		{"Kids to Adults", intPtr(6), false},
		{"Early Childhood", intPtr(3), false},
		{"Everyone", intPtr(0), false},
		{"G", intPtr(0), false},
		{"Unknown", nil, false},
		{"Rating Pending", nil, false},
	}
	for _, tc := range ratingCases {
		payload := `<ComicInfo><Title>Test</Title><AgeRating>` + tc.raw + `</AgeRating></ComicInfo>`
		meta, err := ParseComicInfoXML(strings.NewReader(payload))
		if err != nil {
			t.Fatalf("AgeRating %q failed: %v", tc.raw, err)
		}
		if meta.IsNSFW != tc.wantNSFW {
			t.Errorf("AgeRating %q: IsNSFW=%v, want %v", tc.raw, meta.IsNSFW, tc.wantNSFW)
		}
		if (meta.AgeRatingInt == nil) != (tc.wantInt == nil) || (meta.AgeRatingInt != nil && *meta.AgeRatingInt != *tc.wantInt) {
			t.Errorf("AgeRating %q: AgeRatingInt=%v, want %v", tc.raw, meta.AgeRatingInt, tc.wantInt)
		}
	}

	// Date assembly: YYYY, YYYY-MM, YYYY-MM-DD, and -1 sentinel handling.
	for _, tc := range []struct {
		xml  string
		want string
	}{
		{`<ComicInfo><Year>2024</Year></ComicInfo>`, "2024"},
		{`<ComicInfo><Year>2024</Year><Month>5</Month></ComicInfo>`, "2024-05"},
		{`<ComicInfo><Year>2024</Year><Month>5</Month><Day>9</Day></ComicInfo>`, "2024-05-09"},
		{`<ComicInfo><Year>-1</Year><Month>-1</Month><Day>-1</Day></ComicInfo>`, ""},
	} {
		meta, err := ParseComicInfoXML(strings.NewReader(tc.xml))
		if err != nil {
			t.Fatalf("date case %s failed: %v", tc.xml, err)
		}
		if meta.ReleaseDate != tc.want {
			t.Errorf("date case %s: got %q, want %q", tc.xml, meta.ReleaseDate, tc.want)
		}
	}

	// Manga -> ReadingDirection mappings and Kavita <ISBN> fallback.
	for _, tc := range []struct {
		manga   string
		wantDir string
	}{
		{"Yes", "RIGHT_TO_LEFT"},
		{"YesAndRightToLeft", "RIGHT_TO_LEFT"},
		{"No", "LEFT_TO_RIGHT"},
		{"Unknown", ""},
	} {
		payload := `<ComicInfo><Manga>` + tc.manga + `</Manga><ISBN>9784088820001</ISBN><Number>-1</Number><Volume>-1</Volume><Count>-1</Count></ComicInfo>`
		meta, err := ParseComicInfoXML(strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Manga %q failed: %v", tc.manga, err)
		}
		if meta.ReadingDirection != tc.wantDir || meta.ISBN != "9784088820001" ||
			meta.Number != "" || meta.NumberSort != nil || meta.Volume != "" || meta.Count != nil {
			t.Fatalf("unexpected sentinel/direction parse for %q: %+v", tc.manga, meta)
		}
	}

	// Malformed XML, non-ComicInfo root, XXE entity attempt, and >512 KiB size cap.
	for _, bad := range [][]byte{
		[]byte(`<ComicInfo><Title>unclosed`),
		[]byte(`<OtherRoot><Title>bad</Title></OtherRoot>`),
		[]byte(`<!DOCTYPE foo [<!ENTITY xxe "secret">]><ComicInfo><Title>&xxe;</Title></ComicInfo>`),
		bytes.Repeat([]byte("A"), int(maxEmbeddedMetaBytes)+1),
	} {
		if _, err := ParseComicInfoXML(bytes.NewReader(bad)); err == nil {
			t.Fatalf("expected invalid/oversized ComicInfo XML to error")
		}
	}
}

func intPtr(v int) *int { return &v }
