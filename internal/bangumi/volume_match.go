package bangumi

import (
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// VolumeMatchBook describes a local comic book/archive to match against Bangumi tankobon subjects.
type VolumeMatchBook struct {
	BookID string
	Name   string // 文件名（含或不含扩展名）
	Number string // 可选已有卷号
}

// VolumeMatchResult describes a matched Bangumi tankobon (or oneshot fallback) for a single book.
type VolumeMatchResult struct {
	BookID    string
	SubjectID int64
	CoverURL  string
	NSFW      bool
	NSFWKnown bool
	Title     string
	Date      string
	Summary   string
	ISBN      string
	MatchedBy string // "volume_number" | "title" | "oneshot_fallback"
}

var (
	vmKnownExts = map[string]bool{
		".cbz": true, ".cbr": true, ".cb7": true, ".cbt": true,
		".zip": true, ".rar": true, ".7z": true, ".tar": true,
		".pdf": true, ".epub": true, ".mobi": true,
	}

	vmChapterRe       = regexp.MustCompile(`(?i)(?:第?\s*\d+(?:\.\d+)?\s*[话話回]|(?:^|[^a-z0-9])(?:ch|chap|chapter|ep|episode)\.?[\s_-]*\d+)`)
	vmExplicitVolRe   = regexp.MustCompile(`(?i)(?:第?\s*\d+(?:\.\d+)?\s*[卷册冊集巻]|(?:^|[^a-z0-9])(?:vol\.?|volume|v)[\s._-]*0*\d+)`)
	vmCJKVolRe        = regexp.MustCompile(`第?\s*0*(\d+(?:\.\d+)?)(?:\s*[-~～]\s*0*\d+(?:\.\d+)?)?\s*[卷册冊集巻]`)
	vmVolWordPrefixRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:vol\.?|volume)\.?[\s_-]*0*(\d+(?:\.\d+)?)(?:\s*[-~～]\s*(?:vol\.?|v)?[\s._-]*0*\d+(?:\.\d+)?)?`)
	vmVLetterPrefixRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])v\.?[\s_-]*0*(\d+(?:\.\d+)?)(?:\s*[-~～]\s*v?[\s._-]*0*\d+(?:\.\d+)?)?`)

	vmSeriesCompleteBracketRe = regexp.MustCompile(`(?i)[\[【(（]\s*(?:(?:完结|完結|完|已完结|已完結|未完)|全\s*\d+\s*[卷册冊集巻]?|\d+(?:\s*[-~～]\s*\d+)?\s*[卷册冊集巻]?\s*(?:完|全|未完))\s*[\]】)）]`)
	vmFullOneVolRe            = regexp.MustCompile(`(?:全\s*[一1]\s*[卷册冊]|全\s*0*1\s*[卷册冊集巻])`)
	vmDateNoiseRe             = regexp.MustCompile(`(?:\b(?:19|20)\d{2}[-./年]\d{1,2}(?:[-./月]\d{1,2}日?)?\b|[\[【(（]\s*(?:19|20)\d{2}\s*[\]】)）])`)
	vmResNoiseRe              = regexp.MustCompile(`(?i)(?:\b\d+\s*(?:px|dpi|mb|gb|kb|k)\b|\b\d{3,5}\s*p\b|[\[【(（]\s*\d{2,4}\s*[p页頁]\s*[\]】)）]|\b\d+\s*[x×]\s*\d+\b|\b(?:r-?18|18\+|18禁|c\d{2,3})\b|\brev\s*\d+\b)`)
	vmYearWordRe              = regexp.MustCompile(`(?:^|[^0-9.])(?:19|20)\d{2}(?:$|[^0-9.])`)
	vmTrailingNonNumParenRe   = regexp.MustCompile(`\s*(?:\([^()0-9]*\)|（[^（）0-9]*）)\s*$`)
	vmBracketNumRe            = regexp.MustCompile(`(?:\(|（|\[|【)\s*#?\s*0*(\d+(?:\.\d+)?)(?:\s*[-~～]\s*#?\s*0*\d+(?:\.\d+)?)?\s*(?:\)|）|\]|】)`)
	vmTrailingNumRe           = regexp.MustCompile(`(?:^|[\s_\-#.])0*(\d+(?:\.\d+)?)(?:\s*[-~～]\s*0*\d+(?:\.\d+)?)?\s*$`)
	vmLeadingPureNumRe        = regexp.MustCompile(`^\s*0*(\d+(?:\.\d+)?)(?:\s*[-~～]\s*0*\d+(?:\.\d+)?)?\s*$`)

	vmVolumeRangeRe = regexp.MustCompile(`(?i)\d\s*[-~～]\s*(?:(?:vol(?:ume)?|v)\.?\s*)?\d`)

	vmExtraLatinRe     = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:sp|ex|ova|oad|special|extra|omake|bonus|fanbook|databook|artbook|guidebook|anthology)(?:$|[^a-z])`)
	vmExtraCJKKeywords = []string{
		"番外", "外传", "特典", "小册子", "公式书", "设定集", "画集", "特别篇",
		"短篇集", "前日谈", "后日谈", "导读", "公式ガイド", "ファンブック",
	}

	vmPartBracketRe  = regexp.MustCompile(`(?:\(|（|\[|【)\s*(上|中|下|前篇|后篇)(?:[卷册冊篇])?\s*(?:\)|）|\]|】)`)
	vmPartExplicitRe = regexp.MustCompile(`(?:^|[\s_\-()（）\[\]【】])(?:(上|中|下)[卷册冊]|(前篇|后篇))(?:$|[\s_\-()（）\[\]【】])`)
	vmPartTrailingRe = regexp.MustCompile(`(?:^|[\s_\-])(上|中|下|前篇|后篇)\s*$`)

	vmZeroNumTokenRe = regexp.MustCompile(`([a-z\p{Han}\p{Hiragana}\p{Katakana}])0+(\d+)`)
)

// MatchBooksToTankobons matches local books in a series against Bangumi tankobon subjects
// purely in-memory using filename volume parsing, bonus/extra title matching, and strict
// single-chapter / missing-volume protections.
func MatchBooksToTankobons(
	parentSubjectID int64,
	_ bool,
	books []VolumeMatchBook,
	tankobons []TankobonSubject,
) []VolumeMatchResult {
	if len(books) == 0 {
		return nil
	}

	if len(tankobons) == 0 {
		if len(books) > 1 || parentSubjectID <= 0 {
			return nil
		}
		if isSingleChapterFilename(books[0].Name) {
			return nil
		}
		return []VolumeMatchResult{
			{
				BookID:    books[0].BookID,
				SubjectID: parentSubjectID,
				CoverURL:  CoverEndpointURL(parentSubjectID),
				MatchedBy: "oneshot_fallback",
			},
		}
	}

	parsedBooks := make([]parsedMatchBook, len(books))
	hasMidPart := false
	for i, b := range books {
		pb := parseBookForMatch(b)
		parsedBooks[i] = pb
		if pb.partKind == "中" {
			hasMidPart = true
		}
	}

	parsedTankobons := parseTankobonsForMatch(tankobons)
	nonExtraCount := 0
	maxTankobonVol := 0.0
	for _, pt := range parsedTankobons {
		if pt.partKind == "中" {
			hasMidPart = true
		}
		if !pt.isExtra {
			nonExtraCount++
			if pt.hasVol && pt.vol > maxTankobonVol {
				maxTankobonVol = pt.vol
			}
		}
	}
	lowerPartNum := 2.0
	if hasMidPart || nonExtraCount >= 3 || maxTankobonVol >= 3 {
		lowerPartNum = 3.0
	}

	commonPrefix := detectCommonTankobonPrefix(parsedTankobons)

	type matchCandidate struct {
		result   VolumeMatchResult
		priority int
		tied     bool
	}
	candidates := make(map[int64]matchCandidate)
	var subjectOrder []int64
	addMatch := func(pb parsedMatchBook, idx int, matchedBy string) {
		subject := parsedTankobons[idx].subj
		priority := 1
		if matchedBy == "volume_number" {
			priority = 2
			stem := vmSeriesCompleteBracketRe.ReplaceAllString(pb.stemSimp, " ")
			stem = vmDateNoiseRe.ReplaceAllString(stem, " ")
			if !vmVolumeRangeRe.MatchString(stem) {
				priority = 3
				if vmExplicitVolRe.MatchString(stem) {
					priority = 4
				}
			}
		}
		current, exists := candidates[subject.SubjectID]
		switch {
		case !exists || priority > current.priority:
			candidates[subject.SubjectID] = matchCandidate{
				result: toVolumeMatchResult(pb.book.BookID, subject, matchedBy), priority: priority,
			}
			if !exists {
				subjectOrder = append(subjectOrder, subject.SubjectID)
			}
		case priority == current.priority:
			current.tied = true
			candidates[subject.SubjectID] = current
		}
	}
	for _, pb := range parsedBooks {
		if pb.blockedChapter {
			continue
		}

		if !pb.isExtra {
			if pb.partKind != "" {
				if tIdx := findTankobonByPart(parsedTankobons, pb.partKind); tIdx >= 0 {
					addMatch(pb, tIdx, "volume_number")
					continue
				}
				if !pb.hasVol {
					if partNum, ok := partKindToNumber(pb.partKind, lowerPartNum); ok {
						pb.vol = partNum
						pb.hasVol = true
					}
				}
			}
			if pb.hasVol {
				if tIdx := findTankobonByVolume(parsedTankobons, pb.vol); tIdx >= 0 {
					addMatch(pb, tIdx, "volume_number")
					continue
				}
				// Missing volume: do NOT fall back to shifting positions or fuzzy title matching.
				continue
			}
		}

		if tIdx := findTankobonByTitle(pb, parsedTankobons, commonPrefix); tIdx >= 0 {
			addMatch(pb, tIdx, "title")
		}
	}
	var results []VolumeMatchResult
	for _, id := range subjectOrder {
		if candidate := candidates[id]; !candidate.tied {
			results = append(results, candidate.result)
		}
	}
	return results
}

type parsedMatchBook struct {
	book           VolumeMatchBook
	stemSimp       string
	blockedChapter bool
	isExtra        bool
	vol            float64
	hasVol         bool
	partKind       string
}

type parsedMatchTankobon struct {
	subj     TankobonSubject
	nameCN   string
	nameOrig string
	isExtra  bool
	vol      float64
	hasVol   bool
	partKind string
}

func stripArchiveExt(name string) string {
	trimmed := strings.TrimSpace(name)
	ext := strings.ToLower(filepath.Ext(trimmed))
	if vmKnownExts[ext] {
		return strings.TrimSpace(trimmed[:len(trimmed)-len(ext)])
	}
	return trimmed
}

func isSingleChapterFilename(rawName string) bool {
	stem := vmSeriesCompleteBracketRe.ReplaceAllString(ToSimplified(stripArchiveExt(rawName)), " ")
	if stem == "" {
		return false
	}
	return vmChapterRe.MatchString(stem) && !vmExplicitVolRe.MatchString(stem)
}

// IsVolumeExtra reports whether a volume name identifies supplemental material.
func IsVolumeExtra(name string) bool {
	simp := ToSimplified(name)
	lower := strings.ToLower(simp)
	for _, kw := range vmExtraCJKKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return vmExtraLatinRe.MatchString(lower)
}

func parseBookForMatch(book VolumeMatchBook) parsedMatchBook {
	stem := ToSimplified(stripArchiveExt(book.Name))
	pb := parsedMatchBook{
		book:     book,
		stemSimp: stem,
	}
	if stem == "" {
		if v, ok := parsePositiveVolumeStr(book.Number); ok {
			pb.vol = v
			pb.hasVol = true
		}
		return pb
	}

	if isSingleChapterFilename(book.Name) {
		pb.blockedChapter = true
		return pb
	}

	pb.isExtra = IsVolumeExtra(stem)
	if pb.isExtra {
		return pb
	}

	vol, hasVol, partKind := extractVolumeAndPart(stem, true)
	if !hasVol && partKind == "" {
		if v, ok := parsePositiveVolumeStr(book.Number); ok {
			vol = v
			hasVol = true
		}
	}
	pb.vol = vol
	pb.hasVol = hasVol
	pb.partKind = partKind
	return pb
}

func parseTankobonsForMatch(tankobons []TankobonSubject) []parsedMatchTankobon {
	out := make([]parsedMatchTankobon, len(tankobons))
	for i, t := range tankobons {
		cn := ToSimplified(strings.TrimSpace(t.NameCN))
		orig := ToSimplified(strings.TrimSpace(t.Name))
		pt := parsedMatchTankobon{
			subj:     t,
			nameCN:   cn,
			nameOrig: orig,
			isExtra:  IsVolumeExtra(cn + " " + orig),
		}
		if !pt.isExtra {
			for _, candidate := range []string{cn, orig} {
				if candidate == "" {
					continue
				}
				vol, hasVol, partKind := extractVolumeAndPart(candidate, false)
				if partKind != "" && pt.partKind == "" {
					pt.partKind = partKind
				}
				if hasVol {
					pt.vol = vol
					pt.hasVol = true
					break
				}
			}
		}
		out[i] = pt
	}

	applyMonotonicOrderFallback(out)
	return out
}

func applyMonotonicOrderFallback(items []parsedMatchTankobon) {
	type ordItem struct {
		idx   int
		order int
	}
	var nonExtra []ordItem
	orderCounts := make(map[int]int)
	usedVols := make(map[float64]bool)

	for i, it := range items {
		if it.isExtra {
			continue
		}
		if it.hasVol {
			usedVols[it.vol] = true
		}
		if it.subj.Order > 0 {
			orderCounts[it.subj.Order]++
			nonExtra = append(nonExtra, ordItem{idx: i, order: it.subj.Order})
		}
	}
	if len(nonExtra) == 0 {
		return
	}
	// Require unique positive Order values across non-extra items.
	for _, cnt := range orderCounts {
		if cnt > 1 {
			return
		}
	}
	slices.SortFunc(nonExtra, func(a, b ordItem) int {
		if a.order != b.order {
			return a.order - b.order
		}
		return a.idx - b.idx
	})

	// Explicit volume numbers along the Order sequence must be strictly increasing.
	prevHasVol := false
	prevVol := -1.0
	for _, oi := range nonExtra {
		it := items[oi.idx]
		if it.hasVol {
			if prevHasVol && it.vol <= prevVol {
				return
			}
			prevHasVol = true
			prevVol = it.vol
		}
	}

	for pos, oi := range nonExtra {
		it := &items[oi.idx]
		if it.hasVol || it.partKind != "" || it.subj.Order <= 0 {
			continue
		}
		candVol := float64(it.subj.Order)
		if usedVols[candVol] {
			continue
		}
		consistent := true
		for j := 0; j < pos; j++ {
			prev := items[nonExtra[j].idx]
			if prev.hasVol && prev.vol >= candVol {
				consistent = false
				break
			}
		}
		if consistent {
			for j := pos + 1; j < len(nonExtra); j++ {
				next := items[nonExtra[j].idx]
				if next.hasVol && next.vol <= candVol {
					consistent = false
					break
				}
			}
		}
		if consistent {
			it.vol = candVol
			it.hasVol = true
			usedVols[candVol] = true
		}
	}
}

func extractVolumeAndPart(stem string, stripCompletionTags bool) (vol float64, hasVol bool, partKind string) {
	working := strings.TrimSpace(stem)
	if working == "" {
		return 0, false, ""
	}
	if stripCompletionTags {
		if vmFullOneVolRe.MatchString(working) {
			// Remember 1-volume fallback if no explicit volume number exists elsewhere.
			defer func() {
				if !hasVol && partKind == "" {
					vol = 1
					hasVol = true
				}
			}()
		}
		working = vmSeriesCompleteBracketRe.ReplaceAllString(working, " ")
	} else {
		for {
			next := strings.TrimSpace(vmTrailingNonNumParenRe.ReplaceAllString(working, ""))
			if next == working || next == "" {
				break
			}
			working = next
		}
	}

	// Strip date, year-bracket, resolution, dpi, page count, and revision noise.
	working = vmDateNoiseRe.ReplaceAllString(working, " ")
	working = vmResNoiseRe.ReplaceAllString(working, " ")

	// Priority 1: Explicit CJK volume markers (第X卷, X册, 第X集, etc.).
	if m := vmCJKVolRe.FindStringSubmatch(working); len(m) == 2 {
		if v, ok := parsePositiveVolumeStr(m[1]); ok {
			return v, true, extractPartKind(working)
		}
	}

	// Priority 2: Explicit Vol./Volume prefix.
	if m := vmVolWordPrefixRe.FindStringSubmatch(working); len(m) == 2 {
		if v, ok := parsePositiveVolumeStr(m[1]); ok {
			return v, true, extractPartKind(working)
		}
	}

	// Priority 3: Explicit v<num> prefix (only if no conflicting part marker).
	if m := vmVLetterPrefixRe.FindStringSubmatch(working); len(m) == 2 {
		if v, ok := parsePositiveVolumeStr(m[1]); ok {
			return v, true, extractPartKind(working)
		}
	}

	// Priority 4: Part markers (上/中/下, 前篇/后篇).
	partKind = extractPartKind(working)
	if partKind != "" {
		return 0, false, partKind
	}

	// Priority 5: Parenthesized / bracketed pure volume number (take last valid group).
	if matches := vmBracketNumRe.FindAllStringSubmatch(working, -1); len(matches) > 0 {
		for i := len(matches) - 1; i >= 0; i-- {
			if v, ok := parseVolumeCandidate(matches[i][1]); ok {
				return v, true, ""
			}
		}
	}

	// Priority 6: Trailing standalone number (after stripping non-numeric brackets).
	noBrackets := strings.TrimSpace(squareBracketRe.ReplaceAllString(working, " "))
	noParens := strings.TrimSpace(parenGroupRe.ReplaceAllString(noBrackets, " "))
	noParens = vmYearWordRe.ReplaceAllString(noParens, " ")
	noParens = strings.TrimSpace(noParens)

	if m := vmLeadingPureNumRe.FindStringSubmatch(noParens); len(m) == 2 {
		if v, ok := parseVolumeCandidate(m[1]); ok {
			return v, true, ""
		}
	}
	if loc := vmTrailingNumRe.FindStringSubmatchIndex(noParens); len(loc) == 4 {
		numStart := loc[2]
		if !isFractionOrDateChar(noParens, numStart) {
			if v, ok := parseVolumeCandidate(noParens[loc[2]:loc[3]]); ok {
				return v, true, ""
			}
		}
	}

	return 0, false, ""
}

func isFractionOrDateChar(s string, numStart int) bool {
	if numStart <= 0 {
		return false
	}
	for i := numStart - 1; i >= 0; i-- {
		ch := s[i]
		if ch == ' ' || ch == '\t' {
			continue
		}
		return ch == '/' || ch == ':' || ch == 'x' || ch == 'X'
	}
	return false
}

func extractPartKind(s string) string {
	if m := vmPartBracketRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	if m := vmPartExplicitRe.FindStringSubmatch(s); len(m) == 3 {
		if m[1] != "" {
			return m[1]
		}
		return m[2]
	}
	noBrackets := strings.TrimSpace(squareBracketRe.ReplaceAllString(s, " "))
	noParens := strings.TrimSpace(parenGroupRe.ReplaceAllString(noBrackets, " "))
	if m := vmPartTrailingRe.FindStringSubmatch(noParens); len(m) == 2 {
		return m[1]
	}
	return ""
}

func partKindToNumber(part string, lowerNum float64) (float64, bool) {
	switch part {
	case "上", "前篇":
		return 1, true
	case "中", "后篇":
		return 2, true
	case "下":
		return lowerNum, true
	default:
		return 0, false
	}
}

func parsePositiveVolumeStr(raw string) (float64, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "#"))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= 1000 {
		return 0, false
	}
	return v, true
}

func parseVolumeCandidate(raw string) (float64, bool) {
	v, ok := parsePositiveVolumeStr(raw)
	if !ok {
		return 0, false
	}
	// Exclude bare 4-digit years if any slipped through.
	if v >= 1900 && v <= 2100 && v == math.Trunc(v) {
		return 0, false
	}
	return v, true
}

func findTankobonByPart(items []parsedMatchTankobon, partKind string) int {
	matchIdx := -1
	for i, it := range items {
		if it.isExtra || it.partKind != partKind {
			continue
		}
		if matchIdx < 0 {
			matchIdx = i
		}
	}
	return matchIdx
}

func findTankobonByVolume(items []parsedMatchTankobon, target float64) int {
	for i, it := range items {
		if it.isExtra || !it.hasVol {
			continue
		}
		if math.Abs(it.vol-target) < 1e-6 {
			return i
		}
	}
	return -1
}

func detectCommonTankobonPrefix(items []parsedMatchTankobon) string {
	var baseTitles [][]rune
	for _, it := range items {
		base := it.nameCN
		if base == "" {
			base = it.nameOrig
		}
		base = cleanTitleSurface(base)
		if base != "" {
			baseTitles = append(baseTitles, []rune(base))
		}
	}
	if len(baseTitles) < 2 {
		return ""
	}
	prefix := baseTitles[0]
	for _, cur := range baseTitles[1:] {
		limit := min(len(prefix), len(cur))
		k := 0
		for k < limit && unicode.ToLower(prefix[k]) == unicode.ToLower(cur[k]) {
			k++
		}
		prefix = prefix[:k]
		if len(prefix) < 2 {
			return ""
		}
	}
	trimmed := strings.TrimRight(string(prefix), " \t-–—:：/／_([（【")
	if len([]rune(trimmed)) < 2 {
		return ""
	}
	return trimmed
}

func normalizeVolumeTitleKey(raw string) string {
	s := ToSimplified(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	s = vmZeroNumTokenRe.ReplaceAllString(s, "${1}${2}")
	return NormalizeMatchKey(s)
}

func findTankobonByTitle(pb parsedMatchBook, items []parsedMatchTankobon, commonPrefix string) int {
	bookKeys := collectBookTitleKeys(pb.stemSimp, commonPrefix)
	if len(bookKeys) == 0 {
		return -1
	}

	bestIdx := -1
	bestScore := 0.0
	tie := false

	for i, it := range items {
		if pb.isExtra && !it.isExtra {
			// Regular numbered tankobon should only match an extra book if the tankobon
			// has a clear non-numeric subtitle that matches directly.
			if it.hasVol {
				continue
			}
		}
		tKeys := collectTankobonTitleKeys(it, commonPrefix)
		score := scoreTitleKeySets(bookKeys, tKeys)
		if score <= 0 {
			continue
		}
		if pb.isExtra && it.isExtra {
			score += 0.05
		}
		if score > bestScore+1e-6 {
			bestScore = score
			bestIdx = i
			tie = false
		} else if math.Abs(score-bestScore) <= 1e-6 {
			tie = true
		}
	}

	if bestScore < 0.85 || tie {
		return -1
	}
	return bestIdx
}

func collectBookTitleKeys(stemSimp, commonPrefix string) []string {
	var keys []string
	add := func(raw string) {
		k := normalizeVolumeTitleKey(raw)
		if len([]rune(k)) >= 2 && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}

	cleaned := vmDateNoiseRe.ReplaceAllString(stemSimp, " ")
	cleaned = vmResNoiseRe.ReplaceAllString(cleaned, " ")
	cleaned = vmSeriesCompleteBracketRe.ReplaceAllString(cleaned, " ")

	parsed := ParseSeriesName(cleaned)
	for _, cand := range parsed.Candidates {
		add(cand)
	}

	noBrackets := strings.TrimSpace(squareBracketRe.ReplaceAllString(cleaned, " "))
	add(noBrackets)
	if commonPrefix != "" {
		noPrefix := strings.TrimSpace(strings.TrimPrefix(ToSimplified(noBrackets), commonPrefix))
		noPrefix = strings.TrimLeft(noPrefix, " \t-–—:：/／_")
		add(noPrefix)
	}

	for _, m := range parenGroupRe.FindAllStringSubmatch(noBrackets, -1) {
		inner := firstNonEmpty(m[1], m[2])
		if !isNoiseBracket(inner) {
			add(inner)
		}
	}
	for _, sep := range []string{" - ", " — ", "：", ":", "／", "/", "_"} {
		if strings.Contains(noBrackets, sep) {
			for _, part := range strings.Split(noBrackets, sep) {
				part = strings.TrimSpace(part)
				if !isNoiseBracket(part) {
					add(part)
				}
			}
		}
	}
	return keys
}

func collectTankobonTitleKeys(it parsedMatchTankobon, commonPrefix string) []string {
	var keys []string
	add := func(raw string) {
		k := normalizeVolumeTitleKey(raw)
		if len([]rune(k)) >= 2 && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, raw := range []string{it.nameCN, it.nameOrig} {
		if raw == "" {
			continue
		}
		add(raw)
		if commonPrefix != "" {
			trimmed := strings.TrimSpace(strings.TrimPrefix(raw, commonPrefix))
			trimmed = strings.Trim(trimmed, " \t-–—:：/／_()（）[]【】")
			add(trimmed)
		}
		for _, m := range parenGroupRe.FindAllStringSubmatch(raw, -1) {
			inner := firstNonEmpty(m[1], m[2])
			if !isNoiseBracket(inner) {
				add(inner)
			}
		}
		for _, sep := range []string{" - ", " — ", "：", ":", "／", "/", " "} {
			if strings.Contains(raw, sep) {
				parts := strings.Split(raw, sep)
				if len(parts) >= 2 {
					last := strings.TrimSpace(parts[len(parts)-1])
					if !isNoiseBracket(last) {
						add(last)
					}
				}
			}
		}
	}
	return keys
}

func scoreTitleKeySets(bookKeys, tankobonKeys []string) float64 {
	best := 0.0
	for _, bk := range bookKeys {
		for _, tk := range tankobonKeys {
			if bk == tk {
				return 1.0
			}
			bRunes := []rune(bk)
			tRunes := []rune(tk)
			minLen := min(len(bRunes), len(tRunes))
			maxLen := max(len(bRunes), len(tRunes))
			if minLen >= 3 && maxLen > 0 {
				ratio := float64(minLen) / float64(maxLen)
				if (strings.Contains(tk, bk) || strings.Contains(bk, tk)) && ratio >= 0.6 {
					score := 0.72 + 0.25*ratio
					if score > best {
						best = score
					}
				}
			}
		}
	}
	return best
}

func toVolumeMatchResult(bookID string, t TankobonSubject, matchedBy string) VolumeMatchResult {
	title := strings.TrimSpace(t.NameCN)
	if title == "" {
		title = strings.TrimSpace(t.Name)
	}
	coverURL := strings.TrimSpace(t.CoverURL)
	if coverURL == "" && t.SubjectID > 0 {
		coverURL = CoverEndpointURL(t.SubjectID)
	}
	return VolumeMatchResult{
		BookID:    bookID,
		SubjectID: t.SubjectID,
		CoverURL:  coverURL,
		NSFW:      t.NSFW,
		NSFWKnown: t.NSFWKnown,
		Title:     title,
		Date:      t.Date,
		Summary:   t.Summary,
		ISBN:      t.ISBN,
		MatchedBy: matchedBy,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
