package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

const (
	maxLimit               = 1000
	maxSearchMessagesLimit = 50
	defaultSearchLimit     = 20
	maxContextSnippets     = 5
	defaultBodyChars       = 2000
)

type paginatedResponse[T any] struct {
	Data         []T   `json:"data"`
	Total        int64 `json:"total"`
	TotalMatched int64 `json:"total_matched,omitempty"`
	Returned     int   `json:"returned"`
	Offset       int   `json:"offset"`
	HasMore      bool  `json:"has_more"`
}

func newPaginatedResponse[T any](data []T, total int64, offset int) paginatedResponse[T] {
	if data == nil {
		data = []T{}
	}
	returned := len(data)
	return paginatedResponse[T]{
		Data:         data,
		Total:        total,
		TotalMatched: total,
		Returned:     returned,
		Offset:       offset,
		HasMore:      int64(offset+returned) < total,
	}
}

func searchLimitArg(args map[string]any) int {
	limit := limitArg(args, "limit", defaultSearchLimit)
	if limit > maxSearchMessagesLimit {
		return maxSearchMessagesLimit
	}
	return limit
}

type handlers struct {
	engine         query.Engine
	attachmentsDir string
	dataDir        string

	// Optional vector-search wiring. When hybridEngine is nil, the
	// search_messages handler rejects mode=vector and mode=hybrid with
	// a vector_not_enabled error. backend is additionally required by
	// the find_similar_messages handler to load seed vectors and
	// resolve the active generation.
	hybridEngine *hybrid.Engine
	vectorCfg    vector.Config
	backend      vector.Backend
}

// translateVectorErr maps well-known vector sentinel errors to MCP tool
// error results. Returns nil if the error is not a known sentinel
// (callers should wrap it themselves).
func translateVectorErr(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, vector.ErrNotEnabled):
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured",
		)
	case errors.Is(err, vector.ErrIndexStale):
		return mcp.NewToolResultError(
			"index_stale: the vector index does not match the configured model; " +
				"run `msgvault embeddings build --full-rebuild`",
		)
	case errors.Is(err, vector.ErrIndexBuilding):
		return mcp.NewToolResultError(
			"index_building: the initial vector index is still being built",
		)
	case errors.Is(err, vector.ErrNoActiveGeneration):
		return mcp.NewToolResultError(
			"no_active_generation: vector search has no active index yet; " +
				"run `msgvault embeddings build` to build one",
		)
	case errors.Is(err, vector.ErrEmbeddingTimeout):
		return mcp.NewToolResultError(
			"embedding_timeout: the embedding endpoint did not respond in time; " +
				"retry, or raise [vector.embeddings].timeout in config",
		)
	}
	return nil
}

// getAccountID looks up a source ID by email address.
// Returns nil if account is empty (no filter), or an error if not found.
func (h *handlers) getAccountID(ctx context.Context, account string) (*int64, error) {
	if account == "" {
		return nil, nil //nolint:nilnil // empty input -> no filter, not an error
	}
	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	for _, acc := range accounts {
		if acc.Identifier == account {
			return &acc.ID, nil
		}
	}
	return nil, fmt.Errorf("account not found: %s", account)
}

// getIDArg extracts a required positive integer ID from the arguments map.
func getIDArg(args map[string]any, key string) (int64, error) {
	v, ok := args[key].(float64)
	if !ok {
		return 0, fmt.Errorf("%s parameter is required", key)
	}
	if v != math.Trunc(v) || v < 1 || v > math.MaxInt64 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return int64(v), nil
}

// getDateArg extracts an optional date (YYYY-MM-DD) from the arguments map.
func getDateArg(args map[string]any, key string) (*time.Time, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return nil, nil //nolint:nilnil // absent optional arg is not an error
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return nil, fmt.Errorf("invalid %s date %q: expected YYYY-MM-DD", key, v)
	}
	return &t, nil
}

// readAttachmentFile reads the content-addressed attachment file after
// validating the hash and checking size limits.
func (h *handlers) readAttachmentFile(contentHash string) ([]byte, error) {
	filePath, err := export.StoragePath(h.attachmentsDir, contentHash)
	if err != nil {
		return nil, errors.New("attachment has invalid content hash")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if info.Size() > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", info.Size(), maxAttachmentSize)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentSize+1))
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if int64(len(data)) > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", len(data), maxAttachmentSize)
	}

	return data, nil
}

func (h *handlers) searchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	queryStr, _ := args["query"].(string)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "fts"
	}
	explain, _ := args["explain"].(bool)

	if mode == "vector" || mode == "hybrid" {
		if off := limitArg(args, "offset", 0); off > 0 {
			return mcp.NewToolResultError(
				"pagination_unsupported: mode=" + mode + " only supports offset=0",
			), nil
		}
		return h.searchMessagesHybrid(ctx, args, queryStr, mode, explain)
	}

	if mode != "fts" {
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: must be fts, vector, or hybrid", mode),
		), nil
	}

	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	q := search.Parse(queryStr)
	if sourceID != nil {
		q.AccountIDs = []int64{*sourceID}
	}

	filter := query.MessageFilter{SourceID: sourceID}

	var results []query.MessageSummary
	if len(q.TextTerms) > 0 {
		results, err = h.engine.Search(ctx, q, limit, offset)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}
	}
	if len(results) == 0 {
		results, err = h.engine.SearchFast(ctx, q, filter, limit, offset)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}
	}

	totalMatched, err := h.engine.SearchFastCount(ctx, q, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search count failed: %v", err)), nil
	}

	type searchMessageItem struct {
		query.MessageSummary
		ContextSnippets   []string `json:"context_snippets,omitempty"`
		SnippetsTruncated bool     `json:"snippets_truncated,omitempty"`
	}

	data := make([]searchMessageItem, 0, len(results))
	for _, r := range results {
		item := searchMessageItem{MessageSummary: r}
		if len(q.TextTerms) > 0 {
			msg, err := h.engine.GetMessage(ctx, r.ID)
			if err == nil && msg != nil {
				snippets := extractContext(msg.BodyText, q.TextTerms, 2)
				if len(snippets) > maxContextSnippets {
					item.SnippetsTruncated = true
					snippets = snippets[:maxContextSnippets]
				}
				item.ContextSnippets = snippets
			}
		}
		data = append(data, item)
	}

	return jsonResult(newPaginatedResponse(data, totalMatched, offset))
}

// extractContext finds search terms in body text and returns surrounding context lines.
func extractContext(body string, terms []string, contextLines int) []string {
	if body == "" || len(terms) == 0 {
		return nil
	}
	lines := strings.Split(body, "\n")
	matched := make(map[int]bool)
	var order []int

	for _, term := range terms {
		if len(term) < 2 {
			continue
		}
		lower := strings.ToLower(term)
		for i, line := range lines {
			if !matched[i] && strings.Contains(strings.ToLower(line), lower) {
				start := i - contextLines
				start = max(start, 0)
				end := i + contextLines + 1
				end = min(end, len(lines))
				for j := start; j < end; j++ {
					if !matched[j] {
						matched[j] = true
						order = append(order, j)
					}
				}
			}
		}
	}

	if len(order) == 0 {
		return nil
	}

	sort.Ints(order)
	var snippets []string
	var buf strings.Builder
	prev := -2
	for _, idx := range order {
		if prev >= 0 && idx > prev+1 {
			snippets = append(snippets, buf.String())
			buf.Reset()
		}
		line := lines[idx]
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		prev = idx
	}
	if buf.Len() > 0 {
		snippets = append(snippets, buf.String())
	}
	return snippets
}

// hybridScoreBreakdown exposes fused-score components for debugging.
// All score fields are pointer-typed so "not present in this signal"
// can be distinguished from a legitimate 0.0 score. RRF is omitted in
// mode=vector (only one signal, nothing to fuse).
type hybridScoreBreakdown struct {
	RRF            *float64 `json:"rrf,omitempty"`
	BM25           *float64 `json:"bm25,omitempty"`
	Vector         *float64 `json:"vector,omitempty"`
	SubjectBoosted bool     `json:"subject_boosted,omitempty"`
}

// hybridMessageItem is a single hit in a vector/hybrid response. The
// embedded MessageSummary carries the standard message fields; Score is
// present only when explain=true was requested.
type hybridMessageItem struct {
	query.MessageSummary

	Score *hybridScoreBreakdown `json:"score,omitempty"`
}

// hybridGenerationSummary describes the active vector-index generation
// used to answer a hybrid/vector query.
type hybridGenerationSummary struct {
	ID          int64  `json:"id"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

// hybridSearchResponse is the full response body for a mode=vector or
// mode=hybrid request on the search_messages tool.
type hybridSearchResponse struct {
	Query         string                  `json:"query"`
	Mode          string                  `json:"mode"`
	Returned      int                     `json:"returned"`
	PoolSaturated bool                    `json:"pool_saturated"`
	Generation    hybridGenerationSummary `json:"generation"`
	Messages      []hybridMessageItem     `json:"messages"`
}

// searchMessagesHybrid runs vector or hybrid search via the configured
// hybrid engine. Mirrors api/handlers.go handleHybridSearch: returns
// descriptive errors when the engine is not configured or the index is
// stale/building, otherwise returns RRF-ranked hits hydrated via
// engine.GetMessage.
func (h *handlers) searchMessagesHybrid(
	ctx context.Context, args map[string]any,
	queryStr, mode string, explain bool,
) (*mcp.CallToolResult, error) {
	if h.hybridEngine == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}

	// Resolve account filter to a source ID for the structured Filter.
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := limitArg(args, "limit", 20)
	if maxPage := h.vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && limit > maxPage {
		limit = maxPage
	}

	parsed := search.Parse(queryStr)
	freeText := strings.Join(parsed.TextTerms, " ")

	// mode=vector|hybrid requires at least one free-text term; filter-only
	// queries have no query vector to rank by. Callers that want pure
	// structured filtering should use mode=fts instead.
	if freeText == "" {
		return mcp.NewToolResultError(
			"missing_free_text: mode=" + mode +
				" requires at least one free-text term; use mode=fts for filter-only queries",
		), nil
	}

	subjectTerms := make([]string, 0, len(parsed.TextTerms))
	for _, t := range parsed.TextTerms {
		subjectTerms = append(subjectTerms, strings.ToLower(t))
	}

	filter, err := h.hybridEngine.BuildFilter(ctx, parsed)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("filter resolution failed: %v", err)), nil
	}
	if sourceID != nil {
		filter.SourceIDs = []int64{*sourceID}
	}

	req := hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     freeText,
		Filter:       filter,
		Limit:        limit,
		SubjectTerms: subjectTerms,
		Explain:      explain,
	}

	hits, meta, err := h.hybridEngine.Search(ctx, req)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Bulk-hydrate hits in one round-trip instead of looping
	// GetMessage per result (which fetches body, From, To, Cc, Bcc,
	// labels, and attachments for each id and was the dominant search
	// latency cost).
	hitIDs := make([]int64, len(hits))
	for i, h := range hits {
		hitIDs[i] = h.MessageID
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, hitIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate hybrid hits failed: ids=%d error=%v\n",
			len(hitIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	items := make([]hybridMessageItem, 0, len(hits))
	for _, hit := range hits {
		msg, ok := byID[hit.MessageID]
		if !ok {
			continue
		}
		item := hybridMessageItem{MessageSummary: msg}
		if explain {
			sb := &hybridScoreBreakdown{SubjectBoosted: hit.SubjectBoosted}
			if !math.IsNaN(hit.RRFScore) {
				v := hit.RRFScore
				sb.RRF = &v
			}
			if !math.IsNaN(hit.BM25Score) {
				v := hit.BM25Score
				sb.BM25 = &v
			}
			if !math.IsNaN(hit.VectorScore) {
				v := hit.VectorScore
				sb.Vector = &v
			}
			item.Score = sb
		}
		items = append(items, item)
	}

	return jsonResult(hybridSearchResponse{
		Query:         queryStr,
		Mode:          mode,
		Returned:      len(items),
		PoolSaturated: meta.PoolSaturated,
		Generation: hybridGenerationSummary{
			ID:          int64(meta.Generation.ID),
			Model:       meta.Generation.Model,
			Dimension:   meta.Generation.Dimension,
			Fingerprint: meta.Generation.Fingerprint,
			State:       string(meta.Generation.State),
		},
		Messages: items,
	})
}

// similarMessagesResponse is the full response body for
// find_similar_messages.
type similarMessagesResponse struct {
	SeedMessageID int64                   `json:"seed_message_id"`
	Returned      int                     `json:"returned"`
	Generation    hybridGenerationSummary `json:"generation"`
	Messages      []query.MessageSummary  `json:"messages"`
}

// findSimilarMessages returns nearest-neighbour messages to a seed
// message using the active vector index. The seed is excluded from
// results. Structured filters (account, after, before, has_attachment)
// are applied at the backend level.
func (h *handlers) findSimilarMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.backend == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}
	args := req.GetArguments()

	seedID, err := getIDArg(args, "message_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := limitArg(args, "limit", 20)
	if maxPage := h.vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && limit > maxPage {
		limit = maxPage
	}

	filter, err := h.filterFromFindSimilarArgs(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	seed, err := h.backend.LoadVector(ctx, seedID)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("load seed vector: %v", err)), nil
	}

	active, err := h.backend.ActiveGeneration(ctx)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("active generation: %v", err)), nil
	}

	// +1 so we can drop the seed itself from results without coming up short.
	hits, err := h.backend.Search(ctx, active.ID, seed, limit+1, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Bulk-hydrate keeping rank order. Drop the seed first so the +1
	// over-fetch is paid for in the size budget rather than the
	// hydration round-trip.
	wantIDs := make([]int64, 0, limit)
	for _, hit := range hits {
		if hit.MessageID == seedID {
			continue
		}
		if len(wantIDs) >= limit {
			break
		}
		wantIDs = append(wantIDs, hit.MessageID)
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, wantIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate similar hits failed: ids=%d error=%v\n",
			len(wantIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	messages := make([]query.MessageSummary, 0, len(wantIDs))
	for _, id := range wantIDs {
		if msg, ok := byID[id]; ok {
			messages = append(messages, msg)
		}
	}

	return jsonResult(similarMessagesResponse{
		SeedMessageID: seedID,
		Returned:      len(messages),
		Generation: hybridGenerationSummary{
			ID:          int64(active.ID),
			Model:       active.Model,
			Dimension:   active.Dimension,
			Fingerprint: active.Fingerprint,
			State:       string(active.State),
		},
		Messages: messages,
	})
}

// filterFromFindSimilarArgs builds a vector.Filter from the
// find_similar_messages args. Returns an error if account lookup fails.
// Sender/label filters are intentionally not exposed — resolving
// participant/label names to IDs requires a main-DB handle that the
// MCP handlers struct does not currently hold. A future task that
// wires the DB through can extend both the schema and this helper.
func (h *handlers) filterFromFindSimilarArgs(ctx context.Context, args map[string]any) (vector.Filter, error) {
	var f vector.Filter

	account, _ := args["account"].(string)
	srcID, err := h.getAccountID(ctx, account)
	if err != nil {
		return f, err
	}
	if srcID != nil {
		f.SourceIDs = []int64{*srcID}
	}

	if v, ok := args["has_attachment"].(bool); ok && v {
		tr := true
		f.HasAttachment = &tr
	}
	after, err := getDateArg(args, "after")
	if err != nil {
		return f, err
	}
	if after != nil {
		f.After = after
	}
	before, err := getDateArg(args, "before")
	if err != nil {
		return f, err
	}
	if before != nil {
		f.Before = before
	}
	return f, nil
}

type getMessageResponse struct {
	ID                   int64                  `json:"id"`
	SourceMessageID      string                 `json:"source_message_id"`
	ConversationID       int64                  `json:"conversation_id"`
	SourceConversationID string                 `json:"source_conversation_id"`
	Subject              string                 `json:"subject"`
	MessageType          string                 `json:"message_type,omitempty"`
	Snippet              string                 `json:"snippet"`
	SentAt               time.Time              `json:"sent_at"`
	ReceivedAt           *time.Time             `json:"received_at,omitempty"`
	DeletedAt            *time.Time             `json:"deleted_at,omitempty"`
	SizeEstimate         int64                  `json:"size_estimate"`
	HasAttachments       bool                   `json:"has_attachments"`
	From                 []query.Address        `json:"from"`
	To                   []query.Address        `json:"to"`
	Cc                   []query.Address        `json:"cc"`
	Bcc                  []query.Address        `json:"bcc"`
	BodyText             string                 `json:"body_text"`
	BodyHTML             string                 `json:"body_html"`
	BodyLength           int                    `json:"body_length"`
	Offset               int                    `json:"offset"`
	HasMore              bool                   `json:"has_more"`
	Labels               []string               `json:"labels"`
	Attachments          []query.AttachmentInfo `json:"attachments"`
}

func (h *handlers) getMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}

	bodyOffset := intArg(args, "offset", 0)
	maxChars := limitArg(args, "max_chars", defaultBodyChars)
	if maxChars <= 0 {
		maxChars = defaultBodyChars
	}

	fullBody := msg.BodyText
	bodyLen := len(fullBody)
	if bodyOffset > bodyLen {
		bodyOffset = bodyLen
	}
	end := bodyOffset + maxChars
	if end > bodyLen {
		end = bodyLen
	}

	return jsonResult(getMessageResponse{
		ID:                   msg.ID,
		SourceMessageID:      msg.SourceMessageID,
		ConversationID:       msg.ConversationID,
		SourceConversationID: msg.SourceConversationID,
		Subject:              msg.Subject,
		MessageType:          msg.MessageType,
		Snippet:              msg.Snippet,
		SentAt:               msg.SentAt,
		ReceivedAt:           msg.ReceivedAt,
		DeletedAt:            msg.DeletedAt,
		SizeEstimate:         msg.SizeEstimate,
		HasAttachments:       msg.HasAttachments,
		From:                 msg.From,
		To:                   msg.To,
		Cc:                   msg.Cc,
		Bcc:                  msg.Bcc,
		BodyText:             fullBody[bodyOffset:end],
		BodyHTML:             "",
		BodyLength:           bodyLen,
		Offset:               bodyOffset,
		HasMore:              end < bodyLen,
		Labels:               msg.Labels,
		Attachments:          msg.Attachments,
	})
}

type inMessageMatch struct {
	CharOffset int    `json:"char_offset"`
	Snippet    string `json:"snippet"`
	Line       int    `json:"line"`
}

func (h *handlers) searchInMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	queryStr, _ := args["query"].(string)
	queryStr = strings.TrimSpace(queryStr)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	limit := limitArg(args, "limit", 10)
	offset := limitArg(args, "offset", 0)

	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}

	allMatches := findTermMatches(msg.BodyText, queryStr)
	total := int64(len(allMatches))
	if offset >= len(allMatches) {
		return jsonResult(newPaginatedResponse([]inMessageMatch{}, total, offset))
	}
	end := offset + limit
	if end > len(allMatches) {
		end = len(allMatches)
	}
	return jsonResult(newPaginatedResponse(allMatches[offset:end], total, offset))
}

func findTermMatches(body, term string) []inMessageMatch {
	if body == "" || term == "" {
		return nil
	}
	lowerTerm := strings.ToLower(term)
	lines := strings.Split(body, "\n")
	var matches []inMessageMatch
	byteOffset := 0
	for lineNum, line := range lines {
		lowerLine := strings.ToLower(line)
		searchFrom := 0
		for {
			idx := strings.Index(lowerLine[searchFrom:], lowerTerm)
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			snippet := line
			if len(snippet) > 300 {
				snippet = snippet[:300] + "..."
			}
			matches = append(matches, inMessageMatch{
				CharOffset: byteOffset + pos,
				Snippet:    snippet,
				Line:       lineNum + 1,
			})
			searchFrom = pos + len(term)
		}
		byteOffset += len(line) + 1
	}
	return matches
}

type getMessageAroundResponse struct {
	ID           int64  `json:"id"`
	Phrase       string `json:"phrase"`
	CharOffset   int    `json:"char_offset"`
	BodyText     string `json:"body_text"`
	ContextChars int    `json:"context_chars"`
	BodyLength   int    `json:"body_length"`
}

type getMessageAroundNotFound struct {
	Error      string `json:"error"`
	ID         int64  `json:"id"`
	Phrase     string `json:"phrase"`
	BodyText   string `json:"body_text"`
	BodyLength int    `json:"body_length"`
}

func (h *handlers) getMessageAround(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	phrase, _ := args["phrase"].(string)
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return mcp.NewToolResultError("phrase parameter is required"), nil
	}

	contextChars := limitArg(args, "context_chars", 3000)
	if contextChars <= 0 {
		contextChars = 3000
	}

	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}

	body := msg.BodyText
	bodyLen := len(body)
	charOffset := strings.Index(strings.ToLower(body), strings.ToLower(phrase))
	if charOffset < 0 {
		previewLen := min(500, bodyLen)
		return jsonResult(getMessageAroundNotFound{
			Error:      fmt.Sprintf("phrase %q not found in message body", phrase),
			ID:         id,
			Phrase:     phrase,
			BodyText:   body[:previewLen],
			BodyLength: bodyLen,
		})
	}

	half := (contextChars - len(phrase)) / 2
	if half < 0 {
		half = 0
	}
	start := max(charOffset-half, 0)
	end := min(charOffset+len(phrase)+half, bodyLen)

	return jsonResult(getMessageAroundResponse{
		ID:           id,
		Phrase:       phrase,
		CharOffset:   charOffset,
		BodyText:     body[start:end],
		ContextChars: contextChars,
		BodyLength:   bodyLen,
	})
}

const maxAttachmentSize = 50 * 1024 * 1024 // 50MB

func (h *handlers) getAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "attachment_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	att, err := h.engine.GetAttachment(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get attachment failed: %v", err)), nil
	}
	if att == nil {
		return mcp.NewToolResultError("attachment not found"), nil
	}

	if h.attachmentsDir == "" {
		return mcp.NewToolResultError("attachments directory not configured"), nil
	}

	if att.Size > maxAttachmentSize {
		return mcp.NewToolResultError(fmt.Sprintf("attachment too large: %d bytes (max %d)", att.Size, maxAttachmentSize)), nil
	}

	data, err := h.readAttachmentFile(att.ContentHash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	mimeType := att.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	metaObj := struct {
		Filename string `json:"filename"`
		MimeType string `json:"mime_type"`
		Size     int64  `json:"size"`
	}{
		Filename: att.Filename,
		MimeType: mimeType,
		Size:     att.Size,
	}
	metaJSON, err := json.Marshal(metaObj)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal metadata: %v", err)), nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: "text",
				Text: string(metaJSON),
			},
			mcp.EmbeddedResource{
				Type: "resource",
				Resource: mcp.BlobResourceContents{
					URI:      fmt.Sprintf("attachment:///%d/%s", att.ID, url.PathEscape(att.Filename)),
					MIMEType: mimeType,
					Blob:     base64.StdEncoding.EncodeToString(data),
				},
			},
		},
	}, nil
}

func (h *handlers) exportAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "attachment_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	att, err := h.engine.GetAttachment(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get attachment failed: %v", err)), nil
	}
	if att == nil {
		return mcp.NewToolResultError("attachment not found"), nil
	}

	if h.attachmentsDir == "" {
		return mcp.NewToolResultError("attachments directory not configured"), nil
	}

	if att.Size > maxAttachmentSize {
		return mcp.NewToolResultError(fmt.Sprintf("attachment too large: %d bytes (max %d)", att.Size, maxAttachmentSize)), nil
	}

	data, err := h.readAttachmentFile(att.ContentHash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Determine destination directory.
	destDir, _ := args["destination"].(string)
	if destDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("cannot determine home directory: %v", err)), nil
		}
		destDir = filepath.Join(home, "Downloads")
	}

	info, err := os.Stat(destDir)
	if err != nil || !info.IsDir() {
		return mcp.NewToolResultError("destination directory does not exist: " + destDir), nil //nolint:nilerr // MCP convention: tool errors flow via ToolResultError, not Go error
	}

	// Sanitize and deduplicate filename.
	filename := export.SanitizeFilename(filepath.Base(att.Filename))
	if filename == "" || filename == "." {
		filename = att.ContentHash
	}
	f, outPath, err := export.CreateExclusiveFile(filepath.Join(destDir, filename), 0600)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", err)), nil
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(outPath)
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", writeErr)), nil
	}
	if closeErr != nil {
		_ = os.Remove(outPath)
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", closeErr)), nil
	}

	resp := struct {
		Path     string `json:"path"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}{
		Path:     outPath,
		Filename: filepath.Base(outPath),
		Size:     int64(len(data)),
	}
	return jsonResult(resp)
}

func (h *handlers) listMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	filter := query.MessageFilter{
		SourceID: sourceID,
		Pagination: query.Pagination{
			Limit:  limitArg(args, "limit", 20),
			Offset: limitArg(args, "offset", 0),
		},
	}

	if v, ok := args["from"].(string); ok && v != "" {
		// If it looks like an email address, filter by email; otherwise by display name.
		if strings.Contains(v, "@") || strings.HasPrefix(v, "+") {
			filter.Sender = v
		} else {
			filter.SenderName = v
		}
	}
	if v, ok := args["to"].(string); ok && v != "" {
		filter.Recipient = v
	}
	if v, ok := args["label"].(string); ok && v != "" {
		filter.Label = v
	}
	if v, ok := args["has_attachment"].(bool); ok && v {
		filter.WithAttachmentsOnly = true
	}
	if filter.After, err = getDateArg(args, "after"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if filter.Before, err = getDateArg(args, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if v, ok := args["conversation_id"].(float64); ok && v != 0 {
		v2 := int64(v)
		filter.ConversationID = &v2
	}

	results, err := h.engine.ListMessages(ctx, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list failed: %v", err)), nil
	}

	return jsonResult(results)
}

// getStatsResponse is the JSON body returned by the get_stats MCP tool.
// VectorSearch is omitempty so archives without vector search do not
// surface an empty sub-object to callers.
type getStatsResponse struct {
	Stats        *query.TotalStats   `json:"stats"`
	Accounts     []query.AccountInfo `json:"accounts"`
	VectorSearch *vector.StatsView   `json:"vector_search,omitempty"`
}

func (h *handlers) getStats(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	stats, err := h.engine.GetTotalStats(ctx, query.StatsOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
	}

	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("accounts failed: %v", err)), nil
	}

	// Vector stats are best-effort: partial failures are logged here but
	// still attached to the response so callers see whatever succeeded.
	vs, vsErr := vector.CollectStats(ctx, h.backend)
	if vsErr != nil {
		fmt.Fprintf(os.Stderr, "mcp: vector stats failed: %v\n", vsErr)
	}

	return jsonResult(getStatsResponse{
		Stats:        stats,
		Accounts:     accounts,
		VectorSearch: vs,
	})
}

func (h *handlers) aggregate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	groupBy, _ := args["group_by"].(string)
	if groupBy == "" {
		return mcp.NewToolResultError("group_by parameter is required"), nil
	}

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	opts := query.AggregateOptions{
		SourceID: sourceID,
		Limit:    limitArg(args, "limit", 50),
	}

	if opts.After, err = getDateArg(args, "after"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if opts.Before, err = getDateArg(args, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	viewTypeMap := map[string]query.ViewType{
		"sender":    query.ViewSenders,
		"recipient": query.ViewRecipients,
		"domain":    query.ViewDomains,
		"label":     query.ViewLabels,
		"time":      query.ViewTime,
	}

	viewType, ok := viewTypeMap[groupBy]
	if !ok {
		return mcp.NewToolResultError("invalid group_by: " + groupBy), nil
	}

	rows, err := h.engine.Aggregate(ctx, viewType, opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("aggregate failed: %v", err)), nil
	}

	return jsonResult(rows)
}

// limitArg extracts a non-negative integer limit from a map, with a default.
// JSON numbers arrive as float64. Clamps to maxLimit to prevent excessive
// result sets.
// intArg extracts a non-negative integer from args without the maxLimit clamp
// used by limitArg. Suitable for body-text offsets and similar unbounded values.
func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	if math.IsNaN(v) || v < 0 || math.IsInf(v, 1) || v > float64(math.MaxInt) {
		return def
	}
	return int(v)
}

func limitArg(args map[string]any, key string, def int) int {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if math.IsInf(v, 1) || v > float64(maxLimit) {
		return maxLimit
	}
	return int(v)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// maxStageDeletionResults limits how many messages can be staged in one call.
const maxStageDeletionResults = 100000

func (h *handlers) stageDeletion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Check for query vs structured filters
	queryStr, _ := args["query"].(string)
	queryStr = strings.TrimSpace(queryStr)
	hasQuery := queryStr != ""

	// Check for any structured filter
	fromStr, _ := args["from"].(string)
	domainStr, _ := args["domain"].(string)
	labelStr, _ := args["label"].(string)
	hasAttachment, _ := args["has_attachment"].(bool)
	afterDate, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	beforeDate, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	hasStructuredFilter := fromStr != "" || domainStr != "" || labelStr != "" ||
		hasAttachment || afterDate != nil || beforeDate != nil

	// Validate: must have either query or structured filters, but not both
	if hasQuery && hasStructuredFilter {
		return mcp.NewToolResultError("use either 'query' or structured filters (from, domain, label, etc.), not both"), nil
	}
	if !hasQuery && !hasStructuredFilter {
		return mcp.NewToolResultError("must provide either 'query' or at least one filter (from, domain, label, after, before, has_attachment)"), nil
	}

	var gmailIDs []string
	var description string

	if hasQuery {
		// Query-based search
		q := search.Parse(queryStr)
		if sourceID != nil {
			q.AccountIDs = []int64{*sourceID}
		}

		// Try fast search first
		filter := query.MessageFilter{SourceID: sourceID}
		results, err := h.engine.SearchFast(ctx, q, filter, maxStageDeletionResults, 0)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}

		// Fall back to FTS if no results and query has text terms
		if len(results) == 0 && len(q.TextTerms) > 0 {
			results, err = h.engine.Search(ctx, q, maxStageDeletionResults, 0)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
			}
		}

		for _, msg := range results {
			gmailIDs = append(gmailIDs, msg.SourceMessageID)
		}
		description = "query: " + queryStr
		if len(description) > 50 {
			description = description[:50]
		}
	} else {
		// Structured filter
		filter := query.MessageFilter{
			SourceID:            sourceID,
			Sender:              fromStr,
			Domain:              domainStr,
			Label:               labelStr,
			WithAttachmentsOnly: hasAttachment,
			After:               afterDate,
			Before:              beforeDate,
			Pagination: query.Pagination{
				Limit: maxStageDeletionResults,
			},
		}

		var err error
		gmailIDs, err = h.engine.GetGmailIDsByFilter(ctx, filter)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("filter failed: %v", err)), nil
		}

		// Build description from filters
		var parts []string
		if fromStr != "" {
			parts = append(parts, "from:"+fromStr)
		}
		if domainStr != "" {
			parts = append(parts, "domain:"+domainStr)
		}
		if labelStr != "" {
			parts = append(parts, "label:"+labelStr)
		}
		if hasAttachment {
			parts = append(parts, "has:attachment")
		}
		if afterDate != nil {
			parts = append(parts, "after:"+afterDate.Format("2006-01-02"))
		}
		if beforeDate != nil {
			parts = append(parts, "before:"+beforeDate.Format("2006-01-02"))
		}
		description = "filter: " + strings.Join(parts, " ")
		if len(description) > 50 {
			description = description[:50]
		}
	}

	if len(gmailIDs) == 0 {
		return mcp.NewToolResultError("no messages match the specified criteria"), nil
	}

	// Create deletion manager and manifest
	deletionsDir := filepath.Join(h.dataDir, "deletions")
	manager, err := deletion.NewManager(deletionsDir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create deletion manager: %v", err)), nil
	}

	manifest := deletion.NewManifest(description, gmailIDs)
	manifest.CreatedBy = "mcp"

	// Set filter metadata for execution
	manifest.Filters.Account = account
	if fromStr != "" {
		manifest.Filters.Senders = []string{fromStr}
	}
	if domainStr != "" {
		manifest.Filters.SenderDomains = []string{domainStr}
	}
	if labelStr != "" {
		manifest.Filters.Labels = []string{labelStr}
	}
	if afterDate != nil {
		manifest.Filters.After = afterDate.Format("2006-01-02")
	}
	if beforeDate != nil {
		manifest.Filters.Before = beforeDate.Format("2006-01-02")
	}

	if err := manager.SaveManifest(manifest); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save manifest: %v", err)), nil
	}

	resp := struct {
		BatchID      string `json:"batch_id"`
		MessageCount int    `json:"message_count"`
		Status       string `json:"status"`
		NextStep     string `json:"next_step"`
	}{
		BatchID:      manifest.ID,
		MessageCount: len(gmailIDs),
		Status:       string(manifest.Status),
		NextStep:     "Run 'MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged' to execute deletion (gated for v1), or 'msgvault cancel-deletion " + manifest.ID + "' to cancel",
	}

	return jsonResult(resp)
}

func (h *handlers) searchByDomains(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	domainsStr, _ := args["domains"].(string)
	domainsStr = strings.TrimSpace(domainsStr)
	if domainsStr == "" {
		return mcp.NewToolResultError("domains is required"), nil
	}

	// Split and clean domain list
	var domains []string
	for d := range strings.SplitSeq(domainsStr, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			domains = append(domains, d)
		}
	}
	if len(domains) == 0 {
		return mcp.NewToolResultError("at least one domain is required"), nil
	}

	limit := limitArg(args, "limit", 100)
	offset := limitArg(args, "offset", 0)

	afterDate, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	beforeDate, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	results, err := h.engine.SearchByDomains(ctx, domains, afterDate, beforeDate, limit, offset)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search by domains failed: %v", err)), nil
	}

	return jsonResult(results)
}
