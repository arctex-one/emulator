package sim

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	wire "github.com/arctex-one/client-go/generated"
)

func (a *API) Control() http.Handler {
	mux := http.NewServeMux()
	for _, route := range []string{
		"GET /control/state",
		"PUT /control/settings",
		"PUT /control/tenants/{ref}",
		"POST /control/accounts",
		"PUT /control/scenarios/{ref}",
		"POST /control/transfers/{ref}/step",
		"POST /control/transfers/{ref}/resume",
		"POST /control/deposits",
		"POST /control/deposits/{ref}/resume",
		"POST /control/webhooks/{ref}/retry",
		"POST /control/webhooks/dispatch",
		"POST /control/idempotency/retire",
	} {
		mux.HandleFunc(route, a.controlRequest)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		expected := sha256.Sum256([]byte("Bearer " + a.ControlToken))
		if a.ControlToken == "" || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(supplied[:], expected[:]) != 1 {
			a.respond(w, nil, 401, problem(401, "UNAUTHENTICATED", "A valid control credential is required"))
			return
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			a.respond(w, nil, 404, notFound())
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func controlInput(w http.ResponseWriter, r *http.Request, out any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return problem(413, "INVALID_REQUEST", "Control request too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(out); err != nil {
		return problem(400, "INVALID_REQUEST", "Invalid control request")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return problem(400, "INVALID_REQUEST", "One JSON object required")
	}
	return nil
}
func (a *API) controlRequest(w http.ResponseWriter, r *http.Request) {
	if r.Pattern == "POST /control/webhooks/dispatch" {
		var req struct {
			Event string `json:"event_id"`
		}
		if err := controlInput(w, r, &req); err != nil {
			a.respond(w, nil, 400, err)
			return
		}
		sent, err := a.DeliverOne(r.Context(), req.Event)
		a.respond(w, map[string]bool{"delivered_attempt": sent}, 200, err)
		return
	}
	var result any = map[string]bool{"ok": true}
	err := a.Store.Update(r.Context(), func(s *State) error {
		ref := r.PathValue("ref")
		switch r.Pattern {
		case "GET /control/state":
			result = s.inspect()
		case "PUT /control/settings":
			var req struct {
				Manual    *bool      `json:"manual"`
				Now       *time.Time `json:"now"`
				Advance   string     `json:"advance"`
				WallClock bool       `json:"wall_clock"`
			}
			if err := controlInput(w, r, &req); err != nil {
				return err
			}
			if req.Manual != nil {
				s.Manual = *req.Manual
			}
			if req.Now != nil {
				if s.Clock != nil && req.Now.Before(*s.Clock) {
					return problem(400, "INVALID_REQUEST", "Clock cannot move backwards")
				}
				s.Clock = req.Now
			}
			if req.Advance != "" {
				d, err := time.ParseDuration(req.Advance)
				if err != nil || d < 0 {
					return problem(400, "INVALID_REQUEST", "advance requires a nonnegative duration")
				}
				s.Clock = ptr(s.Now().Add(d))
			}
			if req.WallClock {
				s.Clock = nil
			}
			if !s.Manual {
				for _, ref := range sortedTransferRefs(s) {
					if err := s.run(s.Transfers[ref], true); err != nil {
						return err
					}
				}
			}
		case "PUT /control/tenants/{ref}":
			t := s.tenant(ref)
			if t == nil {
				return notFound()
			}
			var req struct {
				Bearer   *string   `json:"bearer"`
				Actor    *string   `json:"actor_ref"`
				Callback *Callback `json:"callback"`
				Accounts *[]string `json:"accounts"`
			}
			if err := controlInput(w, r, &req); err != nil {
				return err
			}
			if req.Bearer != nil {
				if *req.Bearer == "" {
					return problem(400, "INVALID_REQUEST", "Bearer must be nonempty")
				}
				for _, other := range s.Tenants {
					if other.Ref != t.Ref && other.Bearer == *req.Bearer {
						return problem(409, "INVALID_REQUEST", "Bearer is already assigned")
					}
				}
				t.Bearer = *req.Bearer
			}
			if req.Actor != nil {
				if *req.Actor == "" {
					return problem(400, "INVALID_REQUEST", "actor_ref must be nonempty")
				}
				t.Actor = *req.Actor
			}
			if req.Accounts != nil {
				for _, account := range *req.Accounts {
					ac := s.Accounts[account]
					if ac == nil || ac.Tenant != ref {
						return notFound()
					}
				}
				t.Accounts = *req.Accounts
			}
			if req.Callback != nil {
				if err := validateCallback(*req.Callback); err != nil {
					return err
				}
				t.Callback = *req.Callback
				for _, e := range s.Events {
					if e.Tenant == ref && e.Status == "pending" {
						e.Callback = t.Callback
					}
				}
			}
		case "POST /control/accounts":
			var req struct {
				Tenant  string     `json:"tenant_ref"`
				Ref     string     `json:"account_ref"`
				Asset   wire.Asset `json:"asset"`
				Opening string     `json:"opening_balance"`
			}
			if err := controlInput(w, r, &req); err != nil {
				return err
			}
			t := s.tenant(req.Tenant)
			if t == nil || req.Ref == "" {
				return notFound()
			}
			asset, _, err := resolve(req.Asset)
			if err != nil {
				return err
			}
			value := "0"
			if req.Opening != "0" {
				value, err = positiveAmount(req.Opening, asset)
				if err != nil {
					return err
				}
			}
			if prior := s.Accounts[req.Ref]; prior != nil {
				if prior.Tenant == req.Tenant && sameAsset(prior.Public.Asset, asset) {
					result = prior.Public
					return nil
				}
				return problem(409, "IDEMPOTENCY_CONFLICT", "Account reference already exists")
			}
			s.Accounts[req.Ref] = &Account{Tenant: req.Tenant, Public: wire.Account{AccountRef: req.Ref, Asset: asset, Status: "active"}, Balance: "0"}
			t.Accounts = append(t.Accounts, req.Ref)
			s.post(req.Tenant, req.Ref, "OPENING", []posting{{Amount: value, Kind: "MAIN"}}, raw(wire.AdjustmentOrigin{Type: "ADJUSTMENT", ReasonCode: "emulator_opening"}), s.stamp())
			result = s.Accounts[req.Ref].Public
		case "PUT /control/scenarios/{ref}":
			var req Scenario
			if err := controlInput(w, r, &req); err != nil {
				return err
			}
			if !validScenario(req.Outcome) {
				return problem(400, "INVALID_REQUEST", "Unsupported execution outcome")
			}
			if tr := s.Transfers[ref]; tr != nil {
				if tr.Phase == "done" {
					return problem(409, "INVALID_REQUEST", "Execution is already terminal")
				}
				if (req.Outcome == "pending" || req.Outcome == "fail_before_broadcast") && tr.Phase != "reserve" && tr.Phase != "broadcast" {
					return problem(409, "INVALID_REQUEST", "Execution has already broadcast")
				}
				if (req.Outcome == "fail_after_broadcast" || req.Outcome == "pause_before_finality") && (tr.Phase == "mint" || tr.Phase == "destination_payout") {
					return problem(409, "INVALID_REQUEST", "Source execution has already finalized")
				}
				if req.Outcome == "pause_before_mint" && (tr.Public.Quote.Instructions.Bridge == nil || tr.Phase == "destination_payout") {
					return problem(409, "INVALID_REQUEST", "Mint pause requires an unminted bridge")
				}
				tr.Scenario = req.Outcome
				return nil
			}
			q := s.Quotes[ref]
			if q == nil {
				return notFound()
			}
			if q.Transfer != "" {
				return problem(409, "QUOTE_CLAIM_CONFLICT", "Configure the admitted transfer by its transfer reference")
			}
			if req.Outcome == "pause_before_mint" && q.Public.Instructions.Bridge == nil {
				return problem(400, "INVALID_REQUEST", "Mint pause requires a bridge quote")
			}
			s.Scenarios[ref] = req
		case "POST /control/transfers/{ref}/step", "POST /control/transfers/{ref}/resume":
			t := s.Transfers[ref]
			if t == nil {
				return notFound()
			}
			all := r.Pattern == "POST /control/transfers/{ref}/resume"
			if all {
				t.Scenario = "success"
			}
			if err := s.run(t, all); err != nil {
				return err
			}
			result = t.Public
		case "POST /control/deposits":
			var req DepositInput
			if err := controlInput(w, r, &req); err != nil {
				return err
			}
			var err error
			result, err = s.inject(req)
			return err
		case "POST /control/deposits/{ref}/resume":
			found := false
			for key, dep := range s.Deposits {
				if dep.DepositRef == ref {
					var req DepositInput
					_ = json.Unmarshal([]byte(s.DepositInputs[key]), &req)
					s.completeDeposit(key, req.EventTime)
					result = s.Deposits[key]
					found = true
					break
				}
			}
			if !found {
				return notFound()
			}
		case "POST /control/webhooks/{ref}/retry":
			for _, e := range s.Events {
				if e.ID == ref {
					if e.Status == "sending" {
						return problem(409, "SUBMISSION_IN_PROGRESS", "Delivery is in progress")
					}
					e.Status = "pending"
					e.Next = s.Now()
					e.Callback = s.tenant(e.Tenant).Callback
					return nil
				}
			}
			return notFound()
		case "POST /control/idempotency/retire":
			var req struct {
				Tenant  string `json:"tenant_ref"`
				Surface string `json:"surface"`
				Key     string `json:"key"`
			}
			if err := controlInput(w, r, &req); err != nil {
				return err
			}
			id := string(raw([]string{req.Tenant, req.Surface, req.Key}))
			c, ok := s.Commands[id]
			if !ok {
				return notFound()
			}
			c.Retired = true
			s.Commands[id] = c
		default:
			return notFound()
		}
		return nil
	})
	a.respond(w, result, 200, err)
	a.notify()
}
func validScenario(v string) bool {
	switch v {
	case "success", "fail_before_broadcast", "fail_after_broadcast", "pending", "pause_before_finality", "pause_before_mint":
		return true
	}
	return false
}
func validateCallback(c Callback) error {
	if c.Algorithm != "hmac-sha256" && c.Algorithm != "jws+eddsa-ed25519" {
		return problem(400, "INVALID_REQUEST", "Unsupported callback algorithm")
	}
	if c.Algorithm == "hmac-sha256" && c.Secret == "" {
		return problem(400, "INVALID_REQUEST", "HMAC secret is required")
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return problem(400, "INVALID_REQUEST", "Absolute HTTP callback URL is required")
		}
	}
	return nil
}
func (s *State) inspect() any {
	accounts := map[string]any{}
	for ref, a := range s.Accounts {
		accounts[ref] = map[string]any{"tenant_ref": a.Tenant, "account": a.Public, "balance": a.Balance, "version": fmt.Sprint(a.Version)}
	}
	events := []any{}
	for _, e := range s.Events {
		events = append(events, map[string]any{
			"event_id":        e.ID,
			"tenant_ref":      e.Tenant,
			"type":            e.Type,
			"data":            e.Data,
			"status":          e.Status,
			"attempts":        e.Attempts,
			"next_attempt_at": e.Next,
			"last_status":     e.LastStatus,
			"last_error":      e.LastError,
			"delivery_id":     e.LastDelivery,
		})
	}
	transfers := map[string]any{}
	for ref, t := range s.Transfers {
		transfers[ref] = map[string]any{"tenant_ref": t.Tenant, "transfer": t.Public, "phase": t.Phase, "scenario": t.Scenario, "reservation_held": t.Reserved}
	}
	return map[string]any{"manual": s.Manual, "now": s.Now(), "accounts": accounts, "transfers": transfers, "webhooks": events}
}
