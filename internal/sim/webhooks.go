package sim

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/arctex-one/api-protocol-go/webhook"
	wire "github.com/arctex-one/client-go/generated"
)

func (s *State) verificationKey(t *Tenant, kid string) (any, error) {
	key := ed25519.PrivateKey(t.SigningKey).Public().(ed25519.PublicKey)
	if !webhook.ValidCallbackKeyID(kid) || webhook.CallbackKeyID(key) != kid {
		return nil, notFound()
	}
	return wire.WebhookVerificationKey{Kty: "OKP", Crv: "Ed25519", Alg: "EdDSA", Use: "sig", Kid: kid, X: base64.RawURLEncoding.EncodeToString(key)}, nil
}

type delivery struct {
	Event, ID, URL string
	Body           []byte
	Headers        http.Header
}

func (s *State) prepareDelivery(e *Event) (delivery, error) {
	t := s.tenant(e.Tenant)
	d := delivery{Event: e.ID, ID: s.id("delivery"), URL: e.Callback.URL, Headers: make(http.Header)}
	envelope := struct {
		EventID    string          `json:"event_id"`
		DeliveryID string          `json:"delivery_id"`
		Occurred   string          `json:"occurred_at"`
		Type       string          `json:"type"`
		Data       json.RawMessage `json:"data"`
	}{e.ID, d.ID, e.Occurred, e.Type, e.Data}
	d.Body = raw(envelope)
	stamp := s.stamp()
	d.Headers.Set("Content-Type", "application/json")
	d.Headers.Set("X-Callback-Timestamp", stamp)
	if e.Callback.Algorithm == webhook.AlgorithmEd25519 {
		key := ed25519.PrivateKey(t.SigningKey)
		kid := webhook.CallbackKeyID(key.Public().(ed25519.PublicKey))
		protected, input, err := webhook.Ed25519SigningInput(d.Body, kid, stamp)
		if err != nil {
			return delivery{}, err
		}
		d.Headers.Set("X-Callback-Algorithm", webhook.AlgorithmEd25519)
		d.Headers.Set("X-Callback-Key-Id", kid)
		d.Headers.Set("X-Callback-JWS", webhook.DetachedEd25519(protected, ed25519.Sign(key, input)))
	} else {
		d.Headers.Set("X-Callback-Algorithm", webhook.AlgorithmHMAC)
		d.Headers.Set("X-Callback-Signature", webhook.HMACSignature(d.Body, []byte(e.Callback.Secret), stamp))
	}
	e.Attempts++
	e.Status = "sending"
	e.LastDelivery = d.ID
	return d, nil
}

// DeliverOne commits the attempt before network I/O. It never holds SQLite while
// the client verifies a callback by querying the emulator's key or financial API.
func (a *API) DeliverOne(ctx context.Context, eventID string) (bool, error) {
	var attempt delivery
	err := a.Store.Update(ctx, func(s *State) error {
		for _, e := range s.Events {
			if eventID != "" && e.ID != eventID {
				continue
			}
			if e.Status != "pending" || e.Callback.URL == "" || s.Now().Before(e.Next) {
				continue
			}
			var err error
			attempt, err = s.prepareDelivery(e)
			return err
		}
		if eventID != "" {
			return problem(409, "INVALID_REQUEST", "Event is unavailable or not due for delivery")
		}
		return nil
	})
	if err != nil || attempt.ID == "" {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, attempt.URL, bytes.NewReader(attempt.Body))
	status := 0
	detail := ""
	if err == nil {
		request.Header = attempt.Headers
		var response *http.Response
		response, err = a.Client.Do(request)
		if response != nil {
			status = response.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
		}
	}
	if err != nil {
		detail = "Callback delivery failed"
	}
	// Persist the outcome even if shutdown cancelled the HTTP request. A process
	// interruption here is recovered by Open and keeps the event identity.
	err = a.Store.Update(context.WithoutCancel(ctx), func(s *State) error {
		for _, e := range s.Events {
			if e.ID != attempt.Event || e.LastDelivery != attempt.ID {
				continue
			}
			e.LastStatus = status
			e.LastError = detail
			switch {
			case status >= 200 && status < 300:
				e.Status = "delivered"
			case status == 0 || status == 408 || status == 429 || status >= 500:
				e.Status = "pending"
				if e.Attempts >= 8 {
					e.Status = "exhausted"
				}
				delay := time.Second * time.Duration(1<<min(e.Attempts-1, 6))
				e.Next = s.Now().Add(delay)
			default:
				e.Status = "failed"
			}
			return nil
		}
		return errInvalidState
	})
	return true, err
}
func (a *API) RunWebhooks(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-a.Wake:
		}
		manual := false
		if err := a.Store.Update(ctx, func(s *State) error { manual = s.Manual; return nil }); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if manual {
			continue
		}
		for {
			sent, err := a.DeliverOne(ctx, "")
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if !sent {
				break
			}
		}
	}
}
