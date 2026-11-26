package database

import (
	"strings"
	"unicode"

	"github.com/xJogger/fake-komga-115/internal/cjk"
)

// searchExpr represents a parsed Komga / Koharia search expression.
type searchExpr interface {
	isSearchExpr()
}

type searchOr struct {
	items []searchExpr
}

func (searchOr) isSearchExpr() {}

type searchAnd struct {
	items []searchExpr
}

func (searchAnd) isSearchExpr() {}

type searchNot struct {
	inner searchExpr
}

func (searchNot) isSearchExpr() {}

type searchTerm struct {
	field string // lowercase field prefix or "" for general full-text
	value string
}

func (searchTerm) isSearchExpr() {}

type searchToken struct {
	kind  string // "WORD", "LPAREN", "RPAREN", "AND", "OR", "NOT", "FIELD"
	value string
}

var knownSearchFields = map[string]bool{
	"title": true, "tag": true, "series_tag": true, "book_tag": true,
	"author": true, "writer": true, "penciller": true, "inker": true,
	"colorist": true, "letterer": true, "cover": true, "editor": true,
	"translator": true, "artist": true, "illustrator": true, "conceptor": true,
	"narrator": true, "contributor": true, "publisher": true, "genre": true,
	"language": true, "status": true,
}

func tokenizeSearch(input string) []searchToken {
	var tokens []searchToken
	runes := []rune(strings.TrimSpace(input))
	n := len(runes)
	for i := 0; i < n; {
		r := runes[i]
		if unicode.IsSpace(r) {
			i++
			continue
		}
		if r == '(' {
			tokens = append(tokens, searchToken{kind: "LPAREN", value: "("})
			i++
			continue
		}
		if r == ')' {
			tokens = append(tokens, searchToken{kind: "RPAREN", value: ")"})
			i++
			continue
		}
		if r == '"' {
			i++
			start := i
			for i < n && runes[i] != '"' {
				i++
			}
			val := strings.TrimSpace(string(runes[start:i]))
			if i < n && runes[i] == '"' {
				i++
			}
			if val != "" {
				tokens = append(tokens, searchToken{kind: "WORD", value: val})
			}
			continue
		}
		start := i
		for i < n && !unicode.IsSpace(runes[i]) && runes[i] != '(' && runes[i] != ')' && runes[i] != '"' {
			i++
		}
		word := string(runes[start:i])
		if colon := strings.IndexByte(word, ':'); colon > 0 {
			prefix := strings.ToLower(word[:colon])
			if knownSearchFields[prefix] {
				rest := word[colon+1:]
				tokens = append(tokens, searchToken{kind: "FIELD", value: prefix})
				if rest != "" {
					tokens = append(tokens, searchToken{kind: "WORD", value: rest})
				}
				continue
			}
		}
		switch word {
		case "OR":
			tokens = append(tokens, searchToken{kind: "OR", value: "OR"})
		case "AND":
			tokens = append(tokens, searchToken{kind: "AND", value: "AND"})
		case "NOT":
			tokens = append(tokens, searchToken{kind: "NOT", value: "NOT"})
		default:
			if strings.HasPrefix(word, "-") && len(word) > 1 {
				tokens = append(tokens, searchToken{kind: "NOT", value: "NOT"})
				tokens = append(tokens, searchToken{kind: "WORD", value: word[1:]})
			} else {
				tokens = append(tokens, searchToken{kind: "WORD", value: word})
			}
		}
	}
	return tokens
}

type searchParser struct {
	tokens []searchToken
	pos    int
}

func parseSearchQuery(raw string) searchExpr {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	tokens := tokenizeSearch(raw)
	if len(tokens) == 0 {
		return searchTerm{value: raw}
	}
	p := &searchParser{tokens: tokens}
	expr := p.parseOr("")
	if expr == nil {
		return searchTerm{value: raw}
	}
	return expr
}

func (p *searchParser) peek() (searchToken, bool) {
	if p.pos >= len(p.tokens) {
		return searchToken{}, false
	}
	return p.tokens[p.pos], true
}

func (p *searchParser) next() (searchToken, bool) {
	tok, ok := p.peek()
	if ok {
		p.pos++
	}
	return tok, ok
}

func (p *searchParser) parseOr(defaultField string) searchExpr {
	left := p.parseAnd(defaultField)
	if left == nil {
		return nil
	}
	items := []searchExpr{left}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != "OR" {
			break
		}
		p.next()
		right := p.parseAnd(defaultField)
		if right != nil {
			items = append(items, right)
		}
	}
	if len(items) == 1 {
		return items[0]
	}
	return searchOr{items: items}
}

func (p *searchParser) parseAnd(defaultField string) searchExpr {
	var items []searchExpr
	for {
		tok, ok := p.peek()
		if !ok || tok.kind == "RPAREN" || tok.kind == "OR" {
			break
		}
		if tok.kind == "AND" {
			p.next()
			continue
		}
		atom := p.parseUnary(defaultField)
		if atom == nil {
			break
		}
		items = append(items, atom)
	}
	if len(items) == 0 {
		return nil
	}
	if len(items) == 1 {
		return items[0]
	}
	return searchAnd{items: items}
}

func (p *searchParser) parseUnary(defaultField string) searchExpr {
	tok, ok := p.peek()
	if !ok {
		return nil
	}
	if tok.kind == "NOT" {
		p.next()
		inner := p.parseUnary(defaultField)
		if inner == nil {
			return nil
		}
		return searchNot{inner: inner}
	}
	if tok.kind == "FIELD" {
		p.next()
		field := tok.value
		nextTok, ok := p.peek()
		if !ok {
			return searchTerm{field: defaultField, value: field + ":"}
		}
		if nextTok.kind == "LPAREN" {
			p.next()
			inner := p.parseOr(field)
			if closeTok, ok := p.peek(); ok && closeTok.kind == "RPAREN" {
				p.next()
			}
			return inner
		}
		if nextTok.kind == "WORD" {
			p.next()
			return searchTerm{field: field, value: nextTok.value}
		}
		return nil
	}
	if tok.kind == "LPAREN" {
		p.next()
		inner := p.parseOr(defaultField)
		if closeTok, ok := p.peek(); ok && closeTok.kind == "RPAREN" {
			p.next()
		}
		return inner
	}
	if tok.kind == "WORD" {
		p.next()
		return searchTerm{field: defaultField, value: tok.value}
	}
	p.next()
	return nil
}

// extractPrimarySearchPhrase extracts the clean user phrase (e.g. from "(排球) OR tag:(排球)" -> "排球")
// for relevance ranking and single-query optimization.
func extractPrimarySearchPhrase(raw string) string {
	expr := parseSearchQuery(raw)
	if expr == nil {
		return ""
	}
	var parts []string
	var collect func(e searchExpr)
	collect = func(e searchExpr) {
		switch node := e.(type) {
		case searchTerm:
			if node.field == "" || node.field == "title" {
				parts = append(parts, node.value)
			}
		case searchAnd:
			for _, child := range node.items {
				collect(child)
			}
		case searchOr:
			if len(node.items) > 0 {
				collect(node.items[0])
			}
		}
	}
	collect(expr)
	if len(parts) == 0 {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// buildSeriesSearchSQL builds a parameterized SQL condition for series table alias "s".
func buildSeriesSearchSQL(raw string, useFTS bool) (string, []any) {
	expr := parseSearchQuery(raw)
	if expr == nil {
		return "", nil
	}
	var args []any
	sql := renderSeriesSearchExpr(expr, useFTS, &args)
	if sql == "" {
		return "", nil
	}
	return "(" + sql + ")", args
}

func renderSeriesSearchExpr(expr searchExpr, useFTS bool, args *[]any) string {
	switch node := expr.(type) {
	case searchOr:
		parts := make([]string, 0, len(node.items))
		for _, item := range node.items {
			if sub := renderSeriesSearchExpr(item, useFTS, args); sub != "" {
				parts = append(parts, sub)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	case searchAnd:
		parts := make([]string, 0, len(node.items))
		for _, item := range node.items {
			if sub := renderSeriesSearchExpr(item, useFTS, args); sub != "" {
				parts = append(parts, sub)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "(" + strings.Join(parts, " AND ") + ")"
	case searchNot:
		sub := renderSeriesSearchExpr(node.inner, useFTS, args)
		if sub == "" {
			return ""
		}
		if sub == "0" {
			// Negating an empty/stripped operand (e.g. "NOT ー") must never expand to TRUE.
			return "0"
		}
		return "(NOT " + sub + ")"
	case searchTerm:
		val := strings.TrimSpace(node.value)
		if val == "" {
			return ""
		}
		switch node.field {
		case "language":
			*args = append(*args, val)
			return `EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=s.id AND sm.language=? COLLATE NOCASE)`
		case "status":
			*args = append(*args, strings.ToUpper(val))
			return `EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=s.id AND sm.status=?)`
		}
		folded := cjk.SearchKey(val)
		if folded == "" {
			// Stripped punctuation/quotes/ー must never fail open.
			return "0"
		}
		phrase := cjk.FTSQueryPhrase(val)
		if useFTS && phrase != "" {
			switch node.field {
			case "tag", "series_tag", "book_tag":
				*args = append(*args, "tags_fts : "+phrase)
				return `s.rowid IN (SELECT f.rowid FROM series_search_fts f WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "genre":
				*args = append(*args, "genres_fts : "+phrase)
				return `s.rowid IN (SELECT f.rowid FROM series_search_fts f WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "publisher":
				*args = append(*args, "publisher_fts : "+phrase)
				return `s.rowid IN (SELECT f.rowid FROM series_search_fts f WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "author":
				*args = append(*args, "authors_fts : "+phrase)
				return `s.rowid IN (SELECT f.rowid FROM series_search_fts f WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "writer", "penciller", "inker", "colorist", "letterer", "cover", "editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
				*args = append(*args, "authors_fts : "+phrase, node.field, folded)
				return `s.rowid IN (SELECT f.rowid FROM series_search_fts f WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)) AND EXISTS (SELECT 1 FROM series_authors sa WHERE sa.series_id=s.id AND sa.role=? COLLATE NOCASE AND cjk_contains(sa.name, ?))`
			default:
				*args = append(*args, "{name_fts title_fts alt_fts} : "+phrase)
				return `s.rowid IN (SELECT f.rowid FROM series_search_fts f WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			}
		}
		// Fallback when FTS5 is warming up or the query is pure symbol (e.g. "☆").
		like := "%" + escapeLike(val) + "%"
		switch node.field {
		case "tag", "series_tag", "book_tag":
			*args = append(*args, like)
			return `EXISTS (SELECT 1 FROM series_tags st WHERE st.series_id=s.id AND st.tag LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "genre":
			*args = append(*args, like)
			return `EXISTS (SELECT 1 FROM series_genres sg WHERE sg.series_id=s.id AND sg.genre LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "publisher":
			*args = append(*args, like)
			return `EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=s.id AND sm.publisher LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "author":
			*args = append(*args, like)
			return `EXISTS (SELECT 1 FROM series_authors sa WHERE sa.series_id=s.id AND sa.name LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "writer", "penciller", "inker", "colorist", "letterer", "cover", "editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
			*args = append(*args, like, node.field)
			return `EXISTS (SELECT 1 FROM series_authors sa WHERE sa.series_id=s.id AND sa.name LIKE ? ESCAPE '\' COLLATE NOCASE AND sa.role=? COLLATE NOCASE)`
		default:
			*args = append(*args, like, like, like)
			return `(s.name LIKE ? ESCAPE '\' COLLATE NOCASE OR EXISTS (SELECT 1 FROM series_metadata sm WHERE sm.series_id=s.id AND (sm.title LIKE ? ESCAPE '\' COLLATE NOCASE OR sm.alternate_titles_json LIKE ? ESCAPE '\' COLLATE NOCASE)))`
		}
	}
	return ""
}

// buildBookSearchSQL builds a parameterized SQL condition for books table alias "b".
func buildBookSearchSQL(raw string, useSeriesFTS, useBooksFTS bool) (string, []any) {
	expr := parseSearchQuery(raw)
	if expr == nil {
		return "", nil
	}
	var args []any
	sql := renderBookSearchExpr(expr, useSeriesFTS, useBooksFTS, &args)
	if sql == "" {
		return "", nil
	}
	return "(" + sql + ")", args
}

// Keep sparse metadata searchable while FTS is rebuilding or catching up.
func bookMetadataSearchSQL(bookAlias string) string {
	return `EXISTS (SELECT 1 FROM book_metadata _bm WHERE _bm.book_id=` + bookAlias + `.id AND (
 cjk_contains(_bm.title, ?) OR cjk_contains(_bm.summary, ?)
 OR EXISTS (SELECT 1 FROM json_each(_bm.tags_json) t WHERE cjk_contains(t.value, ?))
 OR EXISTS (SELECT 1 FROM json_each(_bm.authors_json) a WHERE cjk_contains(a.value ->> 'name', ?))
))`
}

func renderBookSearchExpr(expr searchExpr, useSeriesFTS, useBooksFTS bool, args *[]any) string {
	switch node := expr.(type) {
	case searchOr:
		parts := make([]string, 0, len(node.items))
		for _, item := range node.items {
			if sub := renderBookSearchExpr(item, useSeriesFTS, useBooksFTS, args); sub != "" {
				parts = append(parts, sub)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	case searchAnd:
		parts := make([]string, 0, len(node.items))
		for _, item := range node.items {
			if sub := renderBookSearchExpr(item, useSeriesFTS, useBooksFTS, args); sub != "" {
				parts = append(parts, sub)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "(" + strings.Join(parts, " AND ") + ")"
	case searchNot:
		sub := renderBookSearchExpr(node.inner, useSeriesFTS, useBooksFTS, args)
		if sub == "" {
			return ""
		}
		if sub == "0" {
			return "0"
		}
		return "(NOT " + sub + ")"
	case searchTerm:
		val := strings.TrimSpace(node.value)
		if val == "" {
			return ""
		}
		folded := cjk.SearchKey(val)
		if folded == "" {
			return "0"
		}
		phrase := cjk.FTSQueryPhrase(val)
		if useSeriesFTS && phrase != "" {
			switch node.field {
			case "tag", "series_tag", "book_tag":
				*args = append(*args, "tags_fts : "+phrase)
				return `b.series_id IN (SELECT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "genre":
				*args = append(*args, "genres_fts : "+phrase)
				return `b.series_id IN (SELECT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "publisher":
				*args = append(*args, "publisher_fts : "+phrase)
				return `b.series_id IN (SELECT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "author":
				*args = append(*args, "authors_fts : "+phrase)
				return `b.series_id IN (SELECT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid))`
			case "writer", "penciller", "inker", "colorist", "letterer", "cover", "editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
				*args = append(*args, "authors_fts : "+phrase, node.field, folded)
				return `b.series_id IN (SELECT DISTINCT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid JOIN series_authors sa ON sa.series_id=sf.id WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid) AND sa.role=? COLLATE NOCASE AND cjk_contains(sa.name, ?))`
			default:
				if useBooksFTS {
					*args = append(*args, "name_fts : "+phrase, "{name_fts title_fts alt_fts} : "+phrase)
					return `(b.rowid IN (SELECT bf.rowid FROM books_search_fts bf WHERE bf.books_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='b' AND _sd.row_id=bf.rowid)) OR b.series_id IN (SELECT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)))`
				}
				*args = append(*args, folded, folded, folded, folded, folded, "{name_fts title_fts alt_fts} : "+phrase)
				return `(cjk_contains(b.name, ?) OR ` + bookMetadataSearchSQL("b") + ` OR b.series_id IN (SELECT sf.id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)))`
			}
		}
		like := "%" + escapeLike(val) + "%"
		switch node.field {
		case "tag", "series_tag", "book_tag":
			*args = append(*args, like)
			return `b.series_id IN (SELECT st.series_id FROM series_tags st WHERE st.tag LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "genre":
			*args = append(*args, like)
			return `b.series_id IN (SELECT sg.series_id FROM series_genres sg WHERE sg.genre LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "publisher":
			*args = append(*args, like)
			return `b.series_id IN (SELECT sm.series_id FROM series_metadata sm WHERE sm.publisher LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "author":
			*args = append(*args, like)
			return `b.series_id IN (SELECT sa.series_id FROM series_authors sa WHERE sa.name LIKE ? ESCAPE '\' COLLATE NOCASE)`
		case "writer", "penciller", "inker", "colorist", "letterer", "cover", "editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
			*args = append(*args, like, node.field)
			return `b.series_id IN (SELECT sa.series_id FROM series_authors sa WHERE sa.name LIKE ? ESCAPE '\' COLLATE NOCASE AND sa.role=? COLLATE NOCASE)`
		default:
			*args = append(*args, like, folded, folded, folded, folded, like, like, like)
			return `(b.name LIKE ? ESCAPE '\' COLLATE NOCASE OR ` + bookMetadataSearchSQL("b") + ` OR b.series_id IN (SELECT s2.id FROM series s2 WHERE s2.name LIKE ? ESCAPE '\' COLLATE NOCASE) OR b.series_id IN (SELECT sm.series_id FROM series_metadata sm WHERE sm.title LIKE ? ESCAPE '\' COLLATE NOCASE OR sm.alternate_titles_json LIKE ? ESCAPE '\' COLLATE NOCASE))`
		}
	}
	return ""
}

// buildSeriesSearchCandidateSQL builds an FTS5-backed (or covering-index fallback)
// set query returning matching `series_id` values so SeriesPage drives from the
// matched candidate set in a single pass.
func buildSeriesSearchCandidateSQL(raw string, libs []string, useFTS bool) (string, []any, bool) {
	expr := parseSearchQuery(raw)
	if expr == nil {
		return "", nil, false
	}
	var args []any
	sqlText, ok := renderSeriesCandidateSet(expr, libs, useFTS, &args)
	if !ok || sqlText == "" {
		return "", nil, false
	}
	sqlText = wrapSeriesCandidateLibs(sqlText, libs, &args)
	return sqlText, args, true
}

func wrapSeriesCandidateLibs(sqlText string, libs []string, args *[]any) string {
	if len(libs) == 0 {
		return sqlText
	}
	for _, id := range libs {
		*args = append(*args, id)
	}
	return `SELECT _sr.series_id AS series_id FROM (` + sqlText + `) _sr CROSS JOIN series _slib ON _slib.id=_sr.series_id WHERE _slib.library_id IN ` + Placeholders(len(libs))
}

func renderSeriesCandidateSet(expr searchExpr, libs []string, useFTS bool, args *[]any) (string, bool) {
	switch node := expr.(type) {
	case searchOr:
		parts := make([]string, 0, len(node.items))
		for _, item := range node.items {
			sub, ok := renderSeriesCandidateSet(item, libs, useFTS, args)
			if !ok {
				return "", false
			}
			if sub != "" {
				parts = append(parts, "SELECT series_id FROM ("+sub+")")
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		if len(parts) == 1 {
			return parts[0], true
		}
		return strings.Join(parts, " UNION "), true

	case searchAnd:
		var pos, neg []string
		for _, item := range node.items {
			if notNode, isNot := item.(searchNot); isNot {
				sub, ok := renderSeriesCandidateSet(notNode.inner, libs, useFTS, args)
				if !ok {
					return "", false
				}
				if sub != "" {
					neg = append(neg, "SELECT series_id FROM ("+sub+")")
				}
				continue
			}
			sub, ok := renderSeriesCandidateSet(item, libs, useFTS, args)
			if !ok {
				return "", false
			}
			if sub != "" {
				pos = append(pos, "SELECT series_id FROM ("+sub+")")
			}
		}
		if len(pos) == 0 {
			return "", false
		}
		combined := strings.Join(pos, " INTERSECT ")
		for _, n := range neg {
			combined = "SELECT series_id FROM (" + combined + ") EXCEPT " + n
		}
		return combined, true

	case searchNot:
		return "", false

	case searchTerm:
		val := strings.TrimSpace(node.value)
		if val == "" {
			return "", false
		}
		switch node.field {
		case "language":
			*args = append(*args, val)
			return `SELECT sm.series_id AS series_id FROM series_metadata sm WHERE sm.language=? COLLATE NOCASE`, true
		case "status":
			*args = append(*args, strings.ToUpper(val))
			return `SELECT sm.series_id AS series_id FROM series_metadata sm WHERE sm.status=?`, true
		}
		folded := cjk.SearchKey(val)
		if folded == "" {
			return `SELECT id AS series_id FROM series WHERE 0`, true
		}
		phrase := cjk.FTSQueryPhrase(val)
		if useFTS && phrase != "" {
			switch node.field {
			case "tag", "series_tag", "book_tag":
				*args = append(*args, "tags_fts : "+phrase)
				return `SELECT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)`, true
			case "genre":
				*args = append(*args, "genres_fts : "+phrase)
				return `SELECT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)`, true
			case "publisher":
				*args = append(*args, "publisher_fts : "+phrase)
				return `SELECT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)`, true
			case "author":
				*args = append(*args, "authors_fts : "+phrase)
				return `SELECT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)`, true
			case "writer", "penciller", "inker", "colorist", "letterer", "cover", "editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
				*args = append(*args, "authors_fts : "+phrase, node.field, folded)
				return `SELECT DISTINCT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid JOIN series_authors sa ON sa.series_id=sf.id WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid) AND sa.role=? COLLATE NOCASE AND cjk_contains(sa.name, ?)`, true
			default:
				*args = append(*args, "{name_fts title_fts alt_fts} : "+phrase)
				return `SELECT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid)`, true
			}
		}

		like := "%" + escapeLike(val) + "%"
		switch node.field {
		case "tag", "series_tag", "book_tag":
			*args = append(*args, like)
			return `SELECT DISTINCT st.series_id AS series_id FROM series_tags st WHERE st.tag LIKE ? ESCAPE '\' COLLATE NOCASE`, true
		case "genre":
			*args = append(*args, like)
			return `SELECT DISTINCT sg.series_id AS series_id FROM series_genres sg WHERE sg.genre LIKE ? ESCAPE '\' COLLATE NOCASE`, true
		case "publisher":
			*args = append(*args, like)
			return `SELECT sm.series_id AS series_id FROM series_metadata sm WHERE sm.publisher LIKE ? ESCAPE '\' COLLATE NOCASE`, true
		case "author":
			*args = append(*args, like)
			return `SELECT DISTINCT sa.series_id AS series_id FROM series_authors sa WHERE sa.name LIKE ? ESCAPE '\' COLLATE NOCASE`, true
		case "writer", "penciller", "inker", "colorist", "letterer", "cover", "editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
			*args = append(*args, like, node.field)
			return `SELECT DISTINCT sa.series_id AS series_id FROM series_authors sa WHERE sa.name LIKE ? ESCAPE '\' COLLATE NOCASE AND sa.role=? COLLATE NOCASE`, true
		default:
			libWhereS := ""
			if len(libs) > 0 {
				for _, id := range libs {
					*args = append(*args, id)
				}
				libWhereS = "s2.library_id IN " + Placeholders(len(libs)) + " AND "
			}
			*args = append(*args, like)
			b1 := `SELECT s2.id AS series_id FROM series s2 WHERE ` + libWhereS + `s2.name LIKE ? ESCAPE '\' COLLATE NOCASE`
			*args = append(*args, like, like)
			b2 := `SELECT sm.series_id AS series_id FROM series_metadata sm WHERE sm.title LIKE ? ESCAPE '\' COLLATE NOCASE OR sm.alternate_titles_json LIKE ? ESCAPE '\' COLLATE NOCASE`
			return b1 + " UNION " + b2, true
		}
	}
	return "", false
}

// buildBookSearchCandidateSQL builds an FTS5-backed (or covering-index fallback)
// set query returning matching `book_rowid` values.
func buildBookSearchCandidateSQL(raw string, libs []string, useSeriesFTS, useBooksFTS bool) (string, []any, bool) {
	if len(libs) == 0 {
		return "", nil, false
	}
	expr := parseSearchQuery(raw)
	if expr == nil {
		return "", nil, false
	}
	var args []any
	sqlText, ok := renderBookCandidateSet(expr, libs, useSeriesFTS, useBooksFTS, &args)
	if !ok || sqlText == "" {
		return "", nil, false
	}
	return sqlText, args, true
}

type sqlFragment struct {
	sql  string
	args []any
}

func collectBookOrParts(expr searchExpr, libs []string, useSeriesFTS, useBooksFTS bool) ([]sqlFragment, []sqlFragment, bool) {
	switch node := expr.(type) {
	case searchOr:
		var bookParts, seriesParts []sqlFragment
		for _, item := range node.items {
			bp, sp, ok := collectBookOrParts(item, libs, useSeriesFTS, useBooksFTS)
			if !ok {
				return nil, nil, false
			}
			bookParts = append(bookParts, bp...)
			seriesParts = append(seriesParts, sp...)
		}
		return bookParts, seriesParts, len(bookParts) > 0 || len(seriesParts) > 0

	case searchTerm:
		val := strings.TrimSpace(node.value)
		if val == "" {
			return nil, nil, false
		}
		switch node.field {
		case "tag", "series_tag", "book_tag", "genre", "publisher", "language", "status",
			"author", "writer", "penciller", "inker", "colorist", "letterer", "cover",
			"editor", "translator", "artist", "illustrator", "conceptor", "narrator", "contributor":
			var sArgs []any
			seriesSQL, ok := renderSeriesCandidateSet(node, libs, useSeriesFTS, &sArgs)
			if !ok || seriesSQL == "" {
				return nil, nil, false
			}
			return nil, []sqlFragment{{sql: "SELECT series_id FROM (" + seriesSQL + ")", args: sArgs}}, true
		default:
			folded := cjk.SearchKey(val)
			if folded == "" {
				return []sqlFragment{{sql: `SELECT rowid AS book_rowid FROM books WHERE 0`}}, nil, true
			}
			phrase := cjk.FTSQueryPhrase(val)
			var bArgs []any
			var bookSQL string
			if useBooksFTS && phrase != "" {
				bArgs = append(bArgs, "name_fts : "+phrase)
				for _, id := range libs {
					bArgs = append(bArgs, id)
				}
				bookSQL = `SELECT b2.rowid AS book_rowid FROM books_search_fts bf CROSS JOIN books b2 ON b2.rowid=bf.rowid WHERE bf.books_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='b' AND _sd.row_id=bf.rowid) AND b2.library_id IN ` + Placeholders(len(libs))
			} else {
				like := "%" + escapeLike(val) + "%"
				for _, id := range libs {
					bArgs = append(bArgs, id)
				}
				bArgs = append(bArgs, like, folded, folded, folded, folded)
				bookSQL = `SELECT b2.rowid AS book_rowid FROM books b2 WHERE b2.library_id IN ` + Placeholders(len(libs)) + ` AND (b2.name LIKE ? ESCAPE '\' COLLATE NOCASE OR ` + bookMetadataSearchSQL("b2") + `)`
			}
			var sArgs []any
			seriesSQL, ok := renderSeriesCandidateSet(node, libs, useSeriesFTS, &sArgs)
			if !ok || seriesSQL == "" {
				return []sqlFragment{{sql: bookSQL, args: bArgs}}, nil, true
			}
			return []sqlFragment{{sql: bookSQL, args: bArgs}},
				[]sqlFragment{{sql: "SELECT series_id FROM (" + seriesSQL + ")", args: sArgs}}, true
		}

	default:
		var subArgs []any
		sub, ok := renderBookCandidateSet(expr, libs, useSeriesFTS, useBooksFTS, &subArgs)
		if !ok || sub == "" {
			return nil, nil, false
		}
		return []sqlFragment{{sql: "SELECT book_rowid FROM (" + sub + ")", args: subArgs}}, nil, true
	}
}

func renderBookCandidateSet(expr searchExpr, libs []string, useSeriesFTS, useBooksFTS bool, args *[]any) (string, bool) {
	switch node := expr.(type) {
	case searchOr, searchTerm:
		bookFrags, seriesFrags, ok := collectBookOrParts(expr, libs, useSeriesFTS, useBooksFTS)
		if !ok {
			return "", false
		}
		bookParts := make([]string, 0, len(bookFrags)+1)
		for _, f := range bookFrags {
			bookParts = append(bookParts, f.sql)
			*args = append(*args, f.args...)
		}
		if len(seriesFrags) > 0 {
			sSQLs := make([]string, len(seriesFrags))
			for i, f := range seriesFrags {
				sSQLs[i] = f.sql
				*args = append(*args, f.args...)
			}
			sCombined := strings.Join(sSQLs, " UNION ")
			sFiltered := wrapSeriesCandidateLibs(sCombined, libs, args)
			seriesBranch := `SELECT b3.rowid AS book_rowid FROM (` + sFiltered + `) _ms CROSS JOIN books b3 ON b3.series_id=_ms.series_id`
			bookParts = append(bookParts, seriesBranch)
		}
		if len(bookParts) == 0 {
			return "", false
		}
		if len(bookParts) == 1 {
			return bookParts[0], true
		}
		return strings.Join(bookParts, " UNION "), true

	case searchAnd:
		var pos, neg []string
		for _, item := range node.items {
			if notNode, isNot := item.(searchNot); isNot {
				sub, ok := renderBookCandidateSet(notNode.inner, libs, useSeriesFTS, useBooksFTS, args)
				if !ok {
					return "", false
				}
				if sub != "" {
					neg = append(neg, "SELECT book_rowid FROM ("+sub+")")
				}
				continue
			}
			sub, ok := renderBookCandidateSet(item, libs, useSeriesFTS, useBooksFTS, args)
			if !ok {
				return "", false
			}
			if sub != "" {
				pos = append(pos, "SELECT book_rowid FROM ("+sub+")")
			}
		}
		if len(pos) == 0 {
			return "", false
		}
		combined := strings.Join(pos, " INTERSECT ")
		for _, n := range neg {
			combined = "SELECT book_rowid FROM (" + combined + ") EXCEPT " + n
		}
		return combined, true

	case searchNot:
		return "", false
	}
	return "", false
}

// buildFoldedTaxonomySeriesSubquery returns a SQL subquery yielding matching
// `series_id` values for exact CJK-folded taxonomy filters (tags, genres,
// publishers, authors). When series FTS5 is ready, it narrows via the inverted
// index first and verifies exact folded equality with cjk_key().
func buildFoldedTaxonomySeriesSubquery(
	tableAlias, tableName, colName, ftsCol string,
	rawValues []string,
	useFTS bool,
) (string, []any) {
	cleaned := cleanStringSlice(rawValues)
	if len(cleaned) == 0 {
		return "", nil
	}
	folded := make([]string, 0, len(cleaned))
	seenFolded := make(map[string]bool, len(cleaned))
	phrases := make([]string, 0, len(cleaned))
	allHavePhrase := true
	for _, v := range cleaned {
		key := cjk.SearchKey(v)
		if key == "" {
			continue
		}
		if !seenFolded[key] {
			seenFolded[key] = true
			folded = append(folded, key)
		}
		p := cjk.FTSQueryPhrase(v)
		if p == "" {
			allHavePhrase = false
		} else {
			phrases = append(phrases, p)
		}
	}
	if len(folded) == 0 {
		return `SELECT id AS series_id FROM series WHERE 0`, nil
	}
	if useFTS && allHavePhrase && len(phrases) > 0 {
		matchExpr := ftsCol + " : (" + strings.Join(phrases, " OR ") + ")"
		args := make([]any, 0, 1+len(folded))
		args = append(args, matchExpr)
		for _, k := range folded {
			args = append(args, k)
		}
		sql := `SELECT DISTINCT sf.id AS series_id FROM series_search_fts f CROSS JOIN series sf ON sf.rowid=f.rowid JOIN ` +
			tableName + ` ` + tableAlias + ` ON ` + tableAlias + `.series_id=sf.id WHERE f.series_search_fts MATCH ? AND NOT EXISTS (SELECT 1 FROM search_fts_dirty _sd WHERE _sd.kind='s' AND _sd.row_id=f.rowid) AND cjk_key(` +
			tableAlias + `.` + colName + `) IN ` + Placeholders(len(folded))
		return sql, args
	}
	// Fallback when FTS5 is warming up or a filter value is pure symbols.
	args := make([]any, len(cleaned))
	for i, v := range cleaned {
		args[i] = v
	}
	sql := `SELECT DISTINCT ` + tableAlias + `.series_id AS series_id FROM ` +
		tableName + ` ` + tableAlias + ` WHERE ` + tableAlias + `.` + colName + ` COLLATE NOCASE IN ` + Placeholders(len(cleaned))
	return sql, args
}
