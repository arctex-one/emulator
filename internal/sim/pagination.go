package sim

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type cursor struct {
	Scope   string `json:"s"`
	After   int64  `json:"a"`
	Expires int64  `json:"e"`
}

func (s *State) cursorScope(t *Tenant, surface string, filters map[string]string) string {
	accounts := append([]string(nil), t.Accounts...)
	sort.Strings(accounts)
	value := struct {
		Tenant, Actor, Surface string
		Accounts               []string
		Filters                map[string]string
	}{t.Ref, t.Actor, surface, accounts, filters}
	// Preserve the public scope definition including its framed identity hash.
	digest := sha256.Sum256(raw([]string{string(raw(value))}))
	return hex.EncodeToString(digest[:])
}
func (s *State) encodeCursor(scope string, after int64) string {
	body := raw(cursor{scope, after, s.Now().Add(24 * time.Hour).Unix()})
	mac := hmac.New(sha256.New, s.CursorKey)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *State) decodeCursor(token, scope string) (int64, error) {
	malformed := problem(400, "INVALID_REQUEST", "Malformed cursor")
	body, signature, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 {
		return 0, malformed
	}
	rawBody, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return 0, malformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return 0, malformed
	}
	mac := hmac.New(sha256.New, s.CursorKey)
	mac.Write(rawBody)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return 0, problem(410, "RECOVERY_CURSOR_EXPIRED", "Cursor cannot be recovered")
	}
	var c cursor
	if json.Unmarshal(rawBody, &c) != nil || c.After < 0 {
		return 0, malformed
	}
	if c.Scope != scope {
		return 0, problem(400, "INVALID_REQUEST", "Cursor scope differs from authenticated request")
	}
	if s.Now().Unix() >= c.Expires {
		return 0, problem(410, "RECOVERY_CURSOR_EXPIRED", "Cursor expired; reconcile retained history")
	}
	return c.After, nil
}
func (s *State) page(t *Tenant, surface string, q url.Values) (any, error) {
	mappings := map[string]map[string]string{
		"receiving-addresses": {"account_ref": "account"},
		"deposits":            {"account_ref": "account", "receiving_address_ref": "receiving"},
		"transfers":           {"quote_ref": "quote", "transfer_ref": "transfer", "state": "state"},
		"financial-effects":   {"account_ref": "account", "deposit_ref": "deposit", "transfer_ref": "transfer", "journal_entry_ref": "journal"},
	}
	mapping := mappings[surface]
	filters := map[string]string{}
	limit := 50
	for name, values := range q {
		if len(values) != 1 || values[0] == "" {
			return nil, problem(400, "INVALID_REQUEST", "Query parameters must have one nonempty value")
		}
		if name == "limit" {
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 || n > 200 {
				return nil, problem(400, "INVALID_REQUEST", "limit must be between 1 and 200")
			}
			limit = n
			continue
		}
		if name == "cursor" {
			continue
		}
		private, ok := mapping[name]
		if !ok {
			return nil, problem(400, "INVALID_REQUEST", "Unsupported query parameter")
		}
		filters[private] = values[0]
	}
	if account := filters["account"]; account != "" {
		if _, err := s.authorized(t, account); err != nil {
			return nil, err
		}
	}
	if receiving := filters["receiving"]; receiving != "" {
		if _, err := s.read(t, "receiving-addresses", receiving); err != nil {
			return nil, err
		}
	}
	scope := s.cursorScope(t, surface, filters)
	after := int64(0)
	if token := q.Get("cursor"); token != "" {
		var err error
		after, err = s.decodeCursor(token, scope)
		if err != nil {
			return nil, err
		}
	}
	latest := map[string]int64{}
	if surface != "deposits" && surface != "financial-effects" {
		for _, r := range s.Records {
			if r.Surface == surface && r.Tenant == t.Ref {
				latest[r.Ref] = r.Sequence
			}
		}
	}
	rows := []Record{}
	for _, r := range s.Records {
		if r.Surface != surface || r.Tenant != t.Ref || r.Sequence <= after {
			continue
		}
		if _, err := s.authorized(t, r.Account); err != nil {
			continue
		}
		if len(latest) > 0 && latest[r.Ref] != r.Sequence {
			continue
		}
		match := true
		for key, v := range filters {
			actual := map[string]string{"account": r.Account, "receiving": r.Receiving, "quote": r.Quote, "transfer": r.Transfer, "state": r.State, "deposit": r.Deposit, "journal": r.Journal}[key]
			if actual != v {
				match = false
				break
			}
		}
		if match {
			rows = append(rows, r)
			if len(rows) > limit {
				break
			}
		}
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		items = append(items, r.Body)
		after = r.Sequence
	}
	next := s.encodeCursor(scope, after)
	if surface == "deposits" || surface == "financial-effects" {
		return struct {
			Items []json.RawMessage `json:"items"`
			Next  string            `json:"next_cursor"`
			More  bool              `json:"has_more"`
		}{items, next, more}, nil
	}
	var nextPage *string
	if more {
		nextPage = &next
	}
	return struct {
		Items []json.RawMessage `json:"items"`
		Next  *string           `json:"next_cursor,omitempty"`
	}{items, nextPage}, nil
}
