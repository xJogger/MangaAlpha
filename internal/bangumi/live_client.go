package bangumi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/xJogger/fake-komga-115/internal/database"
)

var (
	ErrNSFWBlocked            = errors.New("NSFW Bangumi subjects are disabled")
	ErrSubjectNotFound        = errors.New("Bangumi subject not found")
	ErrCoverUnavailable error = coverUnavailableError{}
)

type coverUnavailableError struct{}

func (coverUnavailableError) Error() string          { return "Bangumi cover unavailable" }
func (coverUnavailableError) CoverUnavailable() bool { return true }

type LiveClient struct {
	baseURL      string
	clientMu     sync.RWMutex
	http         *http.Client
	proxyRaw     string
	limiter      *rate.Limiter
	coverLimiter *rate.Limiter
	readToken    func(context.Context) string
}

func NewLiveClient() *LiveClient {
	tr, _ := NewBangumiTransport("", 20*time.Second)
	return &LiveClient{
		baseURL:      "https://api.bgm.tv",
		http:         newLiveHTTPClient(tr),
		limiter:      rate.NewLimiter(rate.Limit(2.0), 2),
		coverLimiter: rate.NewLimiter(rate.Limit(12.0), 8),
	}
}

func (c *LiveClient) SetProxyURL(proxyURL string) error {
	tr, err := NewBangumiTransport(proxyURL, 20*time.Second)
	if err != nil {
		return err
	}
	c.clientMu.Lock()
	c.http = newLiveHTTPClient(tr)
	c.proxyRaw = proxyURL
	c.clientMu.Unlock()
	return nil
}

func trustedBangumiAPI(u *url.URL) bool {
	return u.Scheme == "https" && u.Hostname() == "api.bgm.tv" && u.User == nil &&
		(u.Port() == "" || u.Port() == "443")
}

func newLiveHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout: 25 * time.Second, Transport: transport, CheckRedirect: checkBangumiRedirect,
	}
}

func checkBangumiRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	for _, previous := range via {
		if req.URL.Host != previous.URL.Host || req.URL.Scheme != previous.URL.Scheme {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			// OAuth POST bodies can carry refresh tokens and client secrets.
			if strings.HasPrefix(via[0].URL.Path, "/oauth/") {
				return http.ErrUseLastResponse
			}
			break
		}
	}
	if !trustedBangumiAPI(req.URL) {
		req.Header.Del("Authorization")
	}
	return nil
}

func setBangumiBearer(req *http.Request, token string) {
	if token != "" && trustedBangumiAPI(req.URL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func showLiveNSFW(ctx context.Context) bool {
	scope, ok := database.UserScopeFromContext(ctx)
	return ok && scope.EffectiveShowNSFW()
}

func (c *LiveClient) bearer(ctx context.Context, target *url.URL) string {
	if c.readToken != nil && trustedBangumiAPI(target) && showLiveNSFW(ctx) {
		return c.readToken(ctx)
	}
	return ""
}

func (c *LiveClient) doRead(req *http.Request, token string, cover bool) (*http.Response, error) {
	setBangumiBearer(req, token)
	client, _ := c.httpClient()
	if cover {
		coverClient := *client
		coverClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if !IsAllowedBangumiCoverURL(next.URL.String()) {
				return http.ErrUseLastResponse
			}
			return checkBangumiRedirect(next, via)
		}
		client = &coverClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		// Redirect and transport errors can include arbitrary upstream strings.
		return nil, errors.New("Bangumi request failed")
	}
	return resp, nil
}

func (c *LiveClient) httpClient() (*http.Client, string) {
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()
	return c.http, c.proxyRaw
}

type apiSubjectResponse struct {
	ID       int64      `json:"id"`
	Type     int        `json:"type"`
	Platform string     `json:"platform"`
	Name     string     `json:"name"`
	NameCN   string     `json:"name_cn"`
	Summary  string     `json:"summary"`
	NSFW     *bool      `json:"nsfw"`
	Series   bool       `json:"series"`
	Date     string     `json:"date"`
	Volumes  int        `json:"volumes"`
	MetaTags []string   `json:"meta_tags"`
	Tags     []TagCount `json:"tags"`
	Images   struct {
		Common string `json:"common"`
		Medium string `json:"medium"`
		Large  string `json:"large"`
	} `json:"images"`
	Rating struct {
		Score float64 `json:"score"`
		Rank  int     `json:"rank"`
	} `json:"rating"`
	Score   float64          `json:"score"`
	Rank    int              `json:"rank"`
	Infobox []apiInfoboxItem `json:"infobox"`
}

type apiInfoboxItem struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type apiPersonRelation struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Relation string `json:"relation"`
}

type apiSubjectRelationItem struct {
	ID       int64  `json:"id"`
	Type     int    `json:"type"`
	Name     string `json:"name"`
	NameCN   string `json:"name_cn"`
	Relation string `json:"relation"`
	NSFW     *bool  `json:"nsfw"`
	Date     string `json:"date"`
	Summary  string `json:"summary"`
	Images   struct {
		Common string `json:"common"`
		Medium string `json:"medium"`
		Large  string `json:"large"`
	} `json:"images"`
}

func (c *LiveClient) Search(
	ctx context.Context,
	keyword string,
	parsed ParsedQuery,
	targetIsOneShot bool,
	limit int,
) ([]SubjectCandidate, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 25 {
		limit = 15
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	allowNSFW := showLiveNSFW(ctx)
	var nsfwFilter any // null includes both classifications for authorized Bangumi accounts.
	if !allowNSFW {
		nsfwFilter = false
	}
	reqBody, _ := json.Marshal(map[string]any{
		"keyword": keyword,
		"filter": map[string]any{
			"type": []int{1},
			"nsfw": nsfwFilter,
		},
	})
	url := fmt.Sprintf("%s/v0/search/subjects?limit=%d", strings.TrimRight(c.baseURL, "/"), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", archiveUserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.doRead(req, c.bearer(ctx, req.URL), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bangumi search API returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []apiSubjectResponse `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, errors.New("invalid Bangumi search response")
	}
	if len(parsed.Candidates) == 0 {
		parsed = ParseSeriesName(keyword)
	}
	out := make([]SubjectCandidate, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.Type != 1 || !allowNSFW && (item.NSFW == nil || *item.NSFW) {
			continue
		}
		cand := mapAPISubject(item, nil)
		score, breakdown := ScoreCandidateDetailed(parsed, cand, targetIsOneShot)
		cand.Confidence = score
		cand.Breakdown = &breakdown
		if len(parsed.compoundTitles) > 0 && cand.Confidence == 0 {
			continue
		}
		out = append(out, cand)
	}
	slices.SortFunc(out, func(a, b SubjectCandidate) int {
		if a.Confidence > b.Confidence {
			return -1
		}
		if a.Confidence < b.Confidence {
			return 1
		}
		return 0
	})
	return out, nil
}

func (c *LiveClient) SubjectByID(ctx context.Context, subjectID int64) (SubjectCandidate, error) {
	item, token, err := c.subject(ctx, subjectID)
	if err != nil {
		return SubjectCandidate{}, err
	}
	persons, _ := c.fetchSubjectPersons(ctx, subjectID, token)
	cand := mapAPISubject(item, persons)
	cand.Confidence = 1.0
	return cand, nil
}

func (c *LiveClient) subject(ctx context.Context, subjectID int64) (apiSubjectResponse, string, error) {
	if subjectID <= 0 {
		return apiSubjectResponse{}, "", ErrSubjectNotFound
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return apiSubjectResponse{}, "", err
	}
	url := fmt.Sprintf("%s/v0/subjects/%d", strings.TrimRight(c.baseURL, "/"), subjectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return apiSubjectResponse{}, "", err
	}
	req.Header.Set("User-Agent", archiveUserAgent)
	req.Header.Set("Accept", "application/json")

	token := c.bearer(ctx, req.URL)
	resp, err := c.doRead(req, token, false)
	if err != nil {
		return apiSubjectResponse{}, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return apiSubjectResponse{}, "", ErrSubjectNotFound
	case http.StatusOK:
	default:
		return apiSubjectResponse{}, "", fmt.Errorf("Bangumi subject API returned HTTP %d", resp.StatusCode)
	}
	var item apiSubjectResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&item); err != nil {
		return apiSubjectResponse{}, "", errors.New("invalid Bangumi subject response")
	}
	if item.ID != subjectID {
		return apiSubjectResponse{}, "", ErrSubjectNotFound
	}
	if !showLiveNSFW(ctx) && (item.NSFW == nil || *item.NSFW) {
		return apiSubjectResponse{}, "", ErrNSFWBlocked
	}
	return item, token, nil
}

// VerifyTankobonRatings fills unknown ratings for aligned subjects using the
// lightweight detail endpoint. Each subject is checked at most once per match.
func (c *LiveClient) VerifyTankobonRatings(ctx context.Context, matches []VolumeMatchResult) {
	type rating struct {
		nsfw, known bool
		coverURL    string
	}
	checked := make(map[int64]rating)
	for i := range matches {
		match := &matches[i]
		if match.NSFWKnown || match.SubjectID <= 0 {
			continue
		}
		result, ok := checked[match.SubjectID]
		if !ok {
			item, _, err := c.subject(ctx, match.SubjectID)
			if err == nil && item.NSFW != nil {
				result = rating{*item.NSFW, true, strings.TrimSpace(item.Images.Common)}
			}
			checked[match.SubjectID] = result
		}
		if result.known {
			match.NSFW, match.NSFWKnown = result.nsfw, true
			if result.coverURL != "" {
				match.CoverURL = result.coverURL
			}
		}
	}
}

func (c *LiveClient) fetchSubjectPersons(ctx context.Context, subjectID int64, token string) ([]apiPersonRelation, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v0/subjects/%d/persons", strings.TrimRight(c.baseURL, "/"), subjectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", archiveUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.doRead(req, token, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var list []apiPersonRelation
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&list); err != nil {
		return nil, nil
	}
	return list, nil
}

// SubjectRelations fetches linked tankobon ("单行本") subjects for subjectID via Bangumi API v0.
func (c *LiveClient) SubjectRelations(ctx context.Context, subjectID int64) ([]TankobonSubject, error) {
	if subjectID <= 0 {
		return nil, ErrSubjectNotFound
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/v0/subjects/%d/subjects", strings.TrimRight(c.baseURL, "/"), subjectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", archiveUserAgent)
	req.Header.Set("Accept", "application/json")

	token := c.bearer(ctx, req.URL)
	resp, err := c.doRead(req, token, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return nil, ErrSubjectNotFound
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("Bangumi subject relations API returned HTTP %d", resp.StatusCode)
	}
	var rawItems []apiSubjectRelationItem
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rawItems); err != nil {
		return nil, errors.New("invalid Bangumi subject relations response")
	}
	allowNSFW := showLiveNSFW(ctx)
	out := make([]TankobonSubject, 0, len(rawItems))
	order := 0
	for _, item := range rawItems {
		if item.ID <= 0 || strings.TrimSpace(item.Relation) != "单行本" {
			continue
		}
		if item.Type != 0 && item.Type != 1 {
			continue
		}
		order++
		isNSFW := item.NSFW != nil && *item.NSFW
		if isNSFW && !allowNSFW {
			continue
		}
		coverURL := strings.TrimSpace(item.Images.Common)
		if coverURL == "" {
			coverURL = strings.TrimSpace(item.Images.Large)
		}
		if coverURL == "" {
			coverURL = CoverEndpointURL(item.ID)
		}
		out = append(out, TankobonSubject{
			SubjectID: item.ID,
			Name:      strings.TrimSpace(item.Name),
			NameCN:    strings.TrimSpace(item.NameCN),
			Date:      strings.TrimSpace(item.Date),
			Summary:   strings.TrimSpace(item.Summary),
			Order:     order,
			NSFW:      isNSFW,
			NSFWKnown: item.NSFW != nil,
			CoverURL:  coverURL,
		})
	}
	return out, nil
}

func (c *LiveClient) FetchCoverBytes(ctx context.Context, coverURL string, subjectID int64) ([]byte, error) {
	target := strings.TrimSpace(coverURL)
	if !showLiveNSFW(ctx) {
		// A local NSFW lock can override the upstream flag. Verify public
		// visibility anonymously before downloading an image for this scope.
		item, _, err := c.subject(ctx, subjectID)
		switch {
		case errors.Is(err, ErrSubjectNotFound), errors.Is(err, ErrNSFWBlocked):
			return nil, ErrCoverUnavailable
		case err != nil:
			return nil, err
		}
		target = item.Images.Common
		if target == "" {
			target = item.Images.Medium
		}
	}
	if !IsAllowedBangumiCoverURL(target) {
		target = ""
	}
	imageEndpoint := strings.TrimRight(c.baseURL, "/") + fmt.Sprintf("/v0/subjects/%d/image?type=common", subjectID)
	if target == "" && subjectID > 0 {
		target = imageEndpoint
	}
	if target == "" {
		return nil, ErrCoverUnavailable
	}
	if err := c.coverLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", archiveUserAgent)
	token := ""
	if target == imageEndpoint {
		token = c.bearer(ctx, req.URL)
	}
	resp, err := c.doRead(req, token, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return nil, ErrCoverUnavailable
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("Bangumi cover download returned HTTP %d", resp.StatusCode)
	}
	if resp.Request != nil && strings.HasPrefix(path.Base(resp.Request.URL.Path), "no_icon_subject") {
		return nil, ErrCoverUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrCoverUnavailable
	}
	return data, nil
}

func mapAPISubject(item apiSubjectResponse, persons []apiPersonRelation) SubjectCandidate {
	var info WikiInfo
	info.Status = "ONGOING"
	for _, entry := range item.Infobox {
		var strVal string
		if err := json.Unmarshal(entry.Value, &strVal); err == nil {
			applyInfoboxSingle(&info, entry.Key, strVal)
			continue
		}
		var arrVal []struct {
			K string `json:"k"`
			V string `json:"v"`
		}
		if err := json.Unmarshal(entry.Value, &arrVal); err == nil {
			var block []string
			for _, pair := range arrVal {
				if pair.K != "" {
					block = append(block, pair.K+"|"+pair.V)
				} else if pair.V != "" {
					block = append(block, pair.V)
				}
			}
			applyInfoboxBlock(&info, entry.Key, block)
		}
	}
	for _, p := range persons {
		rel := strings.TrimSpace(p.Relation)
		switch {
		case rel == "作者":
			info.Authors = append(info.Authors, database.Author{Name: p.Name, Role: "writer"})
			info.Authors = append(info.Authors, database.Author{Name: p.Name, Role: "penciller"})
		case strings.Contains(rel, "原作") || strings.Contains(rel, "脚本"):
			info.Authors = append(info.Authors, database.Author{Name: p.Name, Role: "writer"})
		case strings.Contains(rel, "作画") || strings.Contains(rel, "插图") || strings.Contains(rel, "人物原案"):
			info.Authors = append(info.Authors, database.Author{Name: p.Name, Role: "penciller"})
		}
	}
	info.Authors = dedupeAuthors(info.Authors)

	nameCN := strings.TrimSpace(item.NameCN)
	if nameCN == "" {
		nameCN = info.TitleCN
	}
	platformCode := PlatformCodeFromString(item.Platform)
	if platformCode == 0 && item.Type == 1 {
		platformCode = PlatformComic
	}
	genres, tags := ClassifyTagsAndGenres(item.MetaTags, item.Tags)
	scoreVal := item.Rating.Score
	if scoreVal <= 0 {
		scoreVal = item.Score
	}
	var scorePtr *float64
	if scoreVal > 0 {
		v := scoreVal
		scorePtr = &v
	}
	rank := item.Rating.Rank
	if rank <= 0 {
		rank = item.Rank
	}
	totalVols := info.TotalVolumes
	if totalVols == nil && item.Volumes > 0 {
		v := item.Volumes
		totalVols = &v
	}
	relDate := strings.TrimSpace(item.Date)
	if relDate == "" {
		relDate = info.ReleaseDate
	} else {
		relDate = normalizeDateString(relDate)
	}
	coverURL := item.Images.Common
	if coverURL == "" {
		coverURL = item.Images.Medium
	}
	if coverURL == "" && item.ID > 0 {
		coverURL = CoverEndpointURL(item.ID)
	}
	return SubjectCandidate{
		ID:              item.ID,
		Platform:        platformCode,
		PlatformLabel:   PlatformName(platformCode),
		IsSeries:        item.Series,
		NSFW:            item.NSFW != nil && *item.NSFW,
		Name:            strings.TrimSpace(item.Name),
		NameCN:          nameCN,
		Summary:         strings.TrimSpace(item.Summary),
		Publisher:       info.Publisher,
		Status:          info.Status,
		ReleaseDate:     relDate,
		TotalVolumes:    totalVols,
		Score:           scorePtr,
		Rank:            rank,
		CoverURL:        coverURL,
		Genres:          genres,
		Tags:            tags,
		Authors:         info.Authors,
		AlternateTitles: info.Aliases,
	}
}

func ParseSubjectIDInput(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if idx := strings.Index(raw, "/subject/"); idx >= 0 {
		rest := raw[idx+len("/subject/"):]
		if cut := strings.IndexAny(rest, "/?#"); cut >= 0 {
			rest = rest[:cut]
		}
		raw = rest
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
