package sim

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	wire "github.com/arctex-one/client-go/generated"
	"github.com/arctex-one/emulator/internal/contract"
)

type apiError struct {
	Status        int
	Code, Message string
}

func (e *apiError) Error() string                    { return e.Message }
func problem(status int, code, message string) error { return &apiError{status, code, message} }
func notFound() error                                { return problem(404, "NOT_FOUND", "Resource not found") }

type API struct {
	Store        *Store
	ControlToken string
	requests     atomic.Uint64
	Wake         chan struct{}
	Client       *http.Client
}

func New(store *Store, controlToken string) *API {
	return &API{Store: store, ControlToken: controlToken, Wake: make(chan struct{}, 1), Client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (a *API) notify() {
	select {
	case a.Wake <- struct{}{}:
	default:
	}
}
func (a *API) Public() http.Handler {
	mux := http.NewServeMux()
	for _, route := range []string{
		"GET /v1/assets",
		"GET /v1/accounts",
		"GET /v1/accounts/{account_ref}/balance",
		"GET /v1/accounts/{account_ref}/statement-entries",
		"GET /v1/accounts/{account_ref}/statement-entries/{entry_ref}",
		"GET /v1/receiving-addresses",
		"POST /v1/receiving-addresses",
		"GET /v1/receiving-addresses/{ref}",
		"GET /v1/deposits",
		"GET /v1/deposits/{ref}",
		"POST /v1/quotes",
		"POST /v1/transfers",
		"GET /v1/transfers",
		"GET /v1/transfers/{ref}",
		"GET /v1/bridge-routes",
		"GET /v1/financial-effects",
		"GET /v1/financial-effects/{ref}",
		"GET /v1/webhook-verification-keys/{ref}",
	} {
		mux.HandleFunc(route, a.publicRequest)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			err := a.Store.Update(r.Context(), func(s *State) error {
				if s.authenticate(r.Header.Values("Authorization")) == nil {
					return problem(401, "UNAUTHENTICATED", "A valid bearer credential is required")
				}
				return notFound()
			})
			a.respond(w, nil, 404, err)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *API) publicRequest(w http.ResponseWriter, r *http.Request) {
	var value any
	status := 200
	err := a.Store.Update(r.Context(), func(s *State) error {
		t := s.authenticate(r.Header.Values("Authorization"))
		if t == nil {
			return problem(401, "UNAUTHENTICATED", "A valid bearer credential is required")
		}
		if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
			return problem(400, "INVALID_REQUEST", "Malformed query encoding")
		}
		for _, v := range r.URL.Query() {
			if len(v) != 1 {
				return problem(400, "INVALID_REQUEST", "Repeated query parameters are not supported")
			}
		}
		var err error
		if r.Method == http.MethodPost {
			value, status, err = s.mutate(t, w, r)
			return err
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch path {
		case "assets":
			value = catalog()
		case "accounts":
			items := []wire.Account{}
			for _, ref := range t.Accounts {
				if ac, err := s.authorized(t, ref); err == nil {
					items = append(items, ac.Public)
				}
			}
			value = wire.AccountList{Items: items}
		case "bridge-routes":
			value = bridgeRoutes()
		case "receiving-addresses", "transfers", "deposits", "financial-effects":
			value, err = s.page(t, path, r.URL.Query())
		default:
			if account := r.PathValue("account_ref"); account != "" {
				ac, e := s.authorized(t, account)
				if e != nil {
					return e
				}
				if strings.HasSuffix(path, "/balance") {
					var at *time.Time
					if r.URL.Query().Has("as_of") {
						when, e := time.Parse(time.RFC3339Nano, r.URL.Query().Get("as_of"))
						if e != nil {
							return problem(400, "INVALID_REQUEST", "Invalid as_of")
						}
						at = &when
					}
					value = s.balance(ac, at)
				} else {
					value, err = s.statements(t, account, r.PathValue("entry_ref"), r.URL.Query())
				}
			} else {
				surface, _, _ := strings.Cut(path, "/")
				ref := r.PathValue("ref")
				if surface == "webhook-verification-keys" {
					value, err = s.verificationKey(t, ref)
				} else {
					value, err = s.read(t, surface, ref)
				}
			}
		}
		return err
	})
	a.respond(w, value, status, err)
	a.notify()
}
func (s *State) authenticate(headers []string) *Tenant {
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") {
		return nil
	}
	sum := sha256.Sum256([]byte(strings.TrimPrefix(headers[0], "Bearer ")))
	for i := range s.Tenants {
		t := &s.Tenants[i]
		digest := sha256.Sum256([]byte(t.Bearer))
		if t.Bearer != "" && subtle.ConstantTimeCompare(sum[:], digest[:]) == 1 {
			return t
		}
	}
	return nil
}
func (a *API) respond(w http.ResponseWriter, value any, status int, err error) {
	requestID := fmt.Sprintf("emu_request_%d", a.requests.Add(1))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-ID", requestID)
	if err != nil {
		p := &apiError{503, "DEPENDENCY_UNAVAILABLE", "Simulator storage is temporarily unavailable"}
		var expected *apiError
		if errors.As(err, &expected) {
			p = expected
		}
		status = p.Status
		value = wire.Error{Code: p.Code, Message: p.Message, RequestId: requestID}
		if status == 409 || status == 503 {
			w.Header().Set("Retry-After", "1")
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *State) mutate(t *Tenant, w http.ResponseWriter, r *http.Request) (any, int, error) {
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" {
		return nil, 0, problem(400, "INVALID_REQUEST", "A nonempty Idempotency-Key is required")
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return nil, 0, problem(415, "INVALID_REQUEST", "application/json is required")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return nil, 0, problem(413, "INVALID_REQUEST", "Request exceeds the supported size")
	}
	surface := strings.TrimPrefix(r.URL.Path, "/v1/")
	schema := map[string]string{"quotes": "QuoteRequest", "transfers": "SubmitTransfer", "receiving-addresses": "CreateReceivingAddress"}[surface]
	canonical, err := contract.Request(data, schema)
	if err != nil {
		return nil, 0, problem(400, "INVALID_REQUEST", err.Error())
	}
	identity := string(raw([]string{t.Ref, surface, keys[0]}))
	if prior, ok := s.Commands[identity]; ok {
		if prior.Input != string(canonical) {
			return nil, 0, problem(409, "IDEMPOTENCY_CONFLICT", "The key belongs to different validated input")
		}
		if prior.Retired {
			return nil, 0, problem(410, "IDEMPOTENCY_RECORD_EXPIRED", "Reconcile the original submission")
		}
		if _, err := s.authorized(t, prior.Account); err != nil {
			return nil, 0, err
		}
		return prior.Body, prior.Status, nil
	}
	var value any
	var account string
	status := 200
	ref := s.id(strings.TrimSuffix(surface, "s"))
	switch surface {
	case "quotes":
		var out wire.Quote
		out, err = s.quote(t, ref, canonical)
		value = out
		account = out.Instructions.SourceAccountRef
	case "receiving-addresses":
		var out wire.ReceivingAddress
		out, err = s.receive(t, ref, canonical)
		value = out
		account = out.AccountRef
		status = 201
	case "transfers":
		var out wire.Transfer
		out, err = s.submit(t, ref, canonical)
		value = out
		account = out.Quote.Instructions.SourceAccountRef
		status = 202
	default:
		return nil, 0, notFound()
	}
	if err != nil {
		return nil, 0, err
	}
	body := raw(value)
	s.Commands[identity] = Command{Input: string(canonical), Account: account, Status: status, Body: body}
	return body, status, nil
}
func (s *State) read(t *Tenant, surface, ref string) (any, error) {
	for i := len(s.Records) - 1; i >= 0; i-- {
		r := s.Records[i]
		if r.Tenant != t.Ref || r.Surface != surface || r.Ref != ref {
			continue
		}
		if _, err := s.authorized(t, r.Account); err != nil {
			return nil, err
		}
		if surface == "transfers" {
			tr := s.Transfers[ref]
			out := tr.Public
			out.AccountingRefund = tr.Refund
			return out, nil
		}
		return r.Body, nil
	}
	return nil, notFound()
}
func (s *State) statements(t *Tenant, account, entry string, q url.Values) (any, error) {
	limit, offset := 50, 0
	var err error
	if v := q.Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 200 {
			return nil, problem(400, "INVALID_REQUEST", "Invalid limit")
		}
	}
	if v := q.Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return nil, problem(400, "INVALID_REQUEST", "Invalid offset")
		}
	}
	items := []wire.StatementSummary{}
	index := 0
	for _, r := range s.Records {
		if r.Tenant != t.Ref || r.Surface != "statements" || r.Account != account {
			continue
		}
		var e wire.StatementEntry
		_ = json.Unmarshal(r.Body, &e)
		if entry != "" {
			if e.EntryRef == entry {
				return e, nil
			}
			continue
		}
		if index >= offset && len(items) < limit {
			items = append(items, wire.StatementSummary{EntryRef: e.EntryRef, EntryType: e.EntryType, AccountAmount: e.AccountAmount, EventTime: e.EventTime, Version: e.Version, Asset: e.Asset, EffectRefs: e.EffectRefs})
		}
		index++
	}
	if entry != "" {
		return nil, notFound()
	}
	return wire.StatementList{Items: items}, nil
}
