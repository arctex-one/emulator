package sim

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	consumer "github.com/arctex-one/client-go"
	wire "github.com/arctex-one/client-go/generated"
)

func TestDirectSignedWebhooksRetriesAndStableEffects(t *testing.T) {
	for _, algorithm := range []string{"hmac-sha256", "jws+eddsa-ed25519"} {
		t.Run(algorithm, func(t *testing.T) {
			f := newFixture(t)
			type received struct {
				Event, Delivery, Type string
				Data                  json.RawMessage
			}
			deliveries := []received{}
			var mu sync.Mutex
			reject := true
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				timestamp, err := time.Parse(time.RFC3339Nano, r.Header.Get("X-Callback-Timestamp"))
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if algorithm == "hmac-sha256" {
					err = consumer.VerifyCallback(body, r.Header, []byte("emulator-demo-callback"), timestamp, time.Minute)
				} else {
					client := wire.Client{BaseURL: f.public.URL, Bearer: "emulator-demo", HTTP: f.public.Client()}
					err = consumer.VerifyCallbackWithPublicKey(r.Context(), body, r.Header, &client, timestamp)
				}
				if err != nil {
					t.Error(err)
					w.WriteHeader(401)
					return
				}
				var event struct {
					Event    string          `json:"event_id"`
					Delivery string          `json:"delivery_id"`
					Type     string          `json:"type"`
					Data     json.RawMessage `json:"data"`
				}
				if err = json.Unmarshal(body, &event); err != nil {
					t.Error(err)
				}
				schemas := map[string]string{"transfer.updated": "TransferEvent", "deposit.updated": "DepositEvent", "financial_effect.committed": "FinancialEvent"}
				f.validate(body, schemas[event.Type])
				mu.Lock()
				deliveries = append(deliveries, received{event.Event, event.Delivery, event.Type, event.Data})
				shouldReject := reject
				mu.Unlock()
				if shouldReject {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(204)
			}))
			defer callback.Close()
			f.controlCall("PUT", "/control/tenants/demo", map[string]any{"callback": Callback{URL: callback.URL, Algorithm: algorithm, Secret: "emulator-demo-callback"}}, 200)
			addr := f.receiving()
			f.controlCall("POST", "/control/deposits", DepositInput{Tenant: "demo", Receiving: addr.ReceivingAddressRef, Amount: "5", Event: "signed"}, 200)
			q := f.quote("q", quoteInput())
			tr := f.submit("tr", q)
			f.resume(tr.TransferRef)
			ids := []string{}
			f.store.Update(t.Context(), func(s *State) error {
				for _, e := range s.Events {
					ids = append(ids, e.ID)
				}
				return nil
			})
			types := map[string]bool{}
			for _, id := range ids {
				f.controlCall("POST", "/control/webhooks/dispatch", map[string]string{"event_id": id}, 200)
			}
			f.controlCall("POST", "/control/webhooks/dispatch", map[string]string{"event_id": ids[0]}, 409)
			mu.Lock()
			reject = false
			first := append([]received(nil), deliveries...)
			mu.Unlock()
			f.controlCall("PUT", "/control/settings", map[string]string{"advance": "1s"}, 200)
			for _, id := range ids {
				f.controlCall("POST", "/control/webhooks/dispatch", map[string]string{"event_id": id}, 200)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(deliveries) != 2*len(first) {
				t.Fatal("retry deliveries missing")
			}
			for i, prior := range first {
				retry := deliveries[len(first)+i]
				if prior.Event != retry.Event || prior.Delivery == retry.Delivery || string(prior.Data) != string(retry.Data) {
					t.Fatal("retry changed event identity/data")
				}
				types[prior.Type] = true
				if prior.Type == "financial_effect.committed" {
					var effect wire.FinancialEffect
					json.Unmarshal(prior.Data, &effect)
					canonical := f.call("GET", "/v1/financial-effects/"+effect.EffectRef, "", nil, 200, "FinancialEffect")
					if string(raw(decode[wire.FinancialEffect](t, canonical))) != string(prior.Data) {
						t.Fatal("callback and recovery effect differ")
					}
				}
			}
			if len(types) != 3 {
				t.Fatal("missing webhook type")
			}
		})
	}
}
func TestWebhookPermanentFailureExhaustionAndAdministrativeReplay(t *testing.T) {
	f := newFixture(t)
	status := 400
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer callback.Close()
	f.controlCall("PUT", "/control/tenants/demo", map[string]any{"callback": Callback{URL: callback.URL, Algorithm: "hmac-sha256", Secret: "test"}}, 200)
	q := f.quote("q", quoteInput())
	f.submit("t", q)
	var eventID string
	f.store.Update(t.Context(), func(s *State) error { eventID = s.Events[0].ID; return nil })
	f.controlCall("POST", "/control/webhooks/dispatch", map[string]string{"event_id": eventID}, 200)
	f.store.Update(t.Context(), func(s *State) error {
		if s.Events[0].Status != "failed" {
			t.Fatal("permanent failure retried")
		}
		return nil
	})
	status = 503
	f.controlCall("POST", "/control/webhooks/"+eventID+"/retry", nil, 200)
	for i := 0; i < 7; i++ {
		f.controlCall("POST", "/control/webhooks/dispatch", map[string]string{"event_id": eventID}, 200)
		f.controlCall("PUT", "/control/settings", map[string]string{"advance": "64s"}, 200)
	}
	f.store.Update(t.Context(), func(s *State) error {
		if s.Events[0].Status != "exhausted" {
			t.Fatal("retry horizon never exhausted")
		}
		return nil
	})
	status = 204
	f.controlCall("POST", "/control/webhooks/"+eventID+"/retry", nil, 200)
	f.controlCall("POST", "/control/webhooks/dispatch", map[string]string{"event_id": eventID}, 200)
	f.store.Update(t.Context(), func(s *State) error {
		if s.Events[0].Status != "delivered" || s.Events[0].ID != eventID {
			t.Fatal("administrative replay changed identity")
		}
		return nil
	})
}
func TestDefaultAutomaticCompletionAndWebhookWake(t *testing.T) {
	f := newFixture(t)
	arrived := make(chan struct{}, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		w.WriteHeader(204)
	}))
	defer callback.Close()
	f.controlCall("PUT", "/control/tenants/demo", map[string]any{"callback": Callback{URL: callback.URL, Algorithm: "hmac-sha256", Secret: "test"}}, 200)
	f.controlCall("PUT", "/control/settings", map[string]any{"manual": false, "wall_clock": true}, 200)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.api.RunWebhooks(ctx) }()
	q := f.quote("auto", quoteInput())
	tr := f.submit("auto", q)
	if f.transfer(tr.TransferRef).State != "SETTLED" {
		t.Fatal("default execution did not complete")
	}
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic callback not delivered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestRestartPreservesReservationsCursorsAndInterruptedDeliveries(t *testing.T) {
	f := newFixture(t)
	q := f.quote("q", quoteInput())
	tr := f.submit("t", q)
	f.step(tr.TransferRef)
	page := decode[wire.FinancialEffectList](t, f.call("GET", "/v1/financial-effects?limit=1", "", nil, 200, "FinancialEffectList"))
	cursor := page.NextCursor
	var interrupted string
	f.store.Update(t.Context(), func(s *State) error {
		e := s.Events[0]
		e.Status = "sending"
		e.Attempts = 1
		e.LastDelivery = "interrupted"
		interrupted = e.ID
		return nil
	})
	before := f.accountBalance(quoteInput().SourceAccountRef)
	f.restart()
	if f.accountBalance(quoteInput().SourceAccountRef) != before {
		t.Fatal("reservation lost across restart")
	}
	f.store.Update(t.Context(), func(s *State) error {
		if s.Events[0].Status != "pending" || s.Events[0].ID != interrupted || s.Events[0].Attempts != 1 {
			t.Fatal("interrupted callback lost")
		}
		return nil
	})
	// Repeat the first page after a consumer crash, then continue with its durable cursor.
	replay := decode[wire.FinancialEffectList](t, f.call("GET", "/v1/financial-effects?limit=1", "", nil, 200, "FinancialEffectList"))
	if replay.Items[0].EffectRef != page.Items[0].EffectRef {
		t.Fatal("recovery history changed")
	}
	seen := map[string]bool{page.Items[0].EffectRef: true}
	for {
		next := decode[wire.FinancialEffectList](t, f.call("GET", "/v1/financial-effects?limit=1&cursor="+url.QueryEscape(cursor), "", nil, 200, "FinancialEffectList"))
		for _, effect := range next.Items {
			if seen[effect.EffectRef] {
				t.Fatal("cursor duplicated effect")
			}
			seen[effect.EffectRef] = true
		}
		cursor = next.NextCursor
		if !next.HasMore {
			break
		}
	}
	f.resume(tr.TransferRef)
	f.restart()
	f.resume(tr.TransferRef)
	if f.accountBalance(quoteInput().SourceAccountRef) != before || f.transfer(tr.TransferRef).State != "SETTLED" {
		t.Fatal("restart replay changed settlement")
	}
	if f.submit("t", q).TransferRef != tr.TransferRef {
		t.Fatal("idempotency lost across restart")
	}
}
func (f *fixture) restart() {
	f.t.Helper()
	f.public.Close()
	f.control.Close()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	store, err := Open(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = store
	f.api = New(store, "control")
	f.public = httptest.NewServer(f.api.Public())
	f.control = httptest.NewServer(f.api.Control())
}
