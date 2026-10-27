package agent

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"aico-agent/internal/agent/messages"
)

const (
	historyPageDefaultLimit = 50
	historyPageMaxLimit     = 200
)

// HistoryPage is one window of the Web conversation history. Start, End, and
// Total count raw context messages, including ones the Web view hides, so
// they stay valid as cursors while the session is unchanged. A new SessionID
// means the context was cleared or rewritten and older cursors no longer
// apply.
type HistoryPage struct {
	SessionID string    `json:"session_id"`
	Total     int       `json:"total"`
	Start     int       `json:"start"`
	End       int       `json:"end"`
	HasMore   bool      `json:"has_more"`
	Reset     bool      `json:"reset,omitempty"`
	Messages  []Message `json:"messages"`
}

// historyPageQuery selects a page:
//   - no cursor: the newest Limit messages;
//   - Before: up to Limit messages that end at Before, for scrolling back;
//   - After: messages from After to the end, for incremental refresh.
//
// A cursor is honored only while SessionID, when given, still names the
// active session. Otherwise the newest page is returned with Reset set, and
// the client replaces what it holds.
type historyPageQuery struct {
	Limit     int
	SessionID string
	Before    int
	After     int
	HasBefore bool
	HasAfter  bool
}

// parseHistoryPageQuery reports paged=false when no paging parameter is
// present, so the caller can keep the legacy full-array response.
func parseHistoryPageQuery(values url.Values) (historyPageQuery, bool, error) {
	query := historyPageQuery{Limit: historyPageDefaultLimit}
	paged := false
	for _, key := range []string{"limit", "before", "after", "session_id"} {
		if _, ok := values[key]; ok {
			paged = true
		}
	}
	if !paged {
		return query, false, nil
	}

	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return query, true, fmt.Errorf("limit must be a positive integer")
		}
		if limit > historyPageMaxLimit {
			limit = historyPageMaxLimit
		}
		query.Limit = limit
	}
	parseCursor := func(key string) (int, bool, error) {
		raw := strings.TrimSpace(values.Get(key))
		if raw == "" {
			return 0, false, nil
		}
		cursor, err := strconv.Atoi(raw)
		if err != nil || cursor < 0 {
			return 0, false, fmt.Errorf("%s must be a non-negative integer", key)
		}
		return cursor, true, nil
	}
	var err error
	if query.Before, query.HasBefore, err = parseCursor("before"); err != nil {
		return query, true, err
	}
	if query.After, query.HasAfter, err = parseCursor("after"); err != nil {
		return query, true, err
	}
	if query.HasBefore && query.HasAfter {
		return query, true, fmt.Errorf("before and after cannot be combined")
	}
	query.SessionID = strings.TrimSpace(values.Get("session_id"))
	return query, true, nil
}

// selectRange picks the page range from the live context. It reports reset
// when the request carried a cursor or session that could not be honored, or
// when an incremental refresh fell more than one page behind.
func (q historyPageQuery) selectRange(sessionID string, total int) (start, end int, reset bool) {
	newest := func() (int, int) {
		return max(0, total-q.Limit), total
	}
	if q.SessionID != "" && q.SessionID != sessionID {
		start, end = newest()
		return start, end, true
	}
	switch {
	case q.HasAfter:
		if q.After > total {
			start, end = newest()
			return start, end, true
		}
		if total-q.After > q.Limit {
			start, end = newest()
			return start, end, true
		}
		return q.After, total, false
	case q.HasBefore:
		if q.Before > total {
			start, end = newest()
			return start, end, true
		}
		return max(0, q.Before-q.Limit), q.Before, false
	default:
		start, end = newest()
		return start, end, false
	}
}

func (s *Server) webHistoryPage(query historyPageQuery) HistoryPage {
	var end int
	var reset, selected bool
	window, history := s.webHistoryWindow(func(sessionID string, view []messages.Message) (int, int) {
		var start int
		start, end, reset = query.selectRange(sessionID, len(view))
		selected = true
		return start, end
	})
	if !selected {
		// No active session could be read, so a cursor or session the client
		// holds cannot be honored either.
		reset = query.HasBefore || query.HasAfter || query.SessionID != ""
	}
	if history == nil {
		history = []Message{}
	}
	end = min(end, window.Total)
	return HistoryPage{
		SessionID: window.SessionID,
		Total:     window.Total,
		Start:     window.Start,
		End:       end,
		HasMore:   window.Start > 0,
		Reset:     reset,
		Messages:  history,
	}
}

// webHistoryTurnSnapshot returns the history of the latest turn: messages
// from the last user message to the end, at most historyPageMaxLimit. Chat
// responses carry this instead of the whole conversation; clients that need
// more read /api/history.
func (s *Server) webHistoryTurnSnapshot() []Message {
	_, history := s.webHistoryWindow(func(_ string, view []messages.Message) (int, int) {
		start := max(0, len(view)-historyPageMaxLimit)
		for i := len(view) - 1; i >= start; i-- {
			if view[i].Role == messages.MessageRoleUser {
				return i, len(view)
			}
		}
		return start, len(view)
	})
	return history
}
