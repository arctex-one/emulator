package sim

import (
	"encoding/json"
	wire "github.com/arctex-one/client-go/generated"
	"github.com/shopspring/decimal"
	"time"
)

func (s *State) quote(t *Tenant, ref string, input []byte) (wire.Quote, error) {
	var req wire.QuoteRequest
	_ = json.Unmarshal(input, &req)
	account, err := s.authorized(t, req.SourceAccountRef)
	if err != nil {
		return wire.Quote{}, err
	}
	source, scale, err := resolve(req.SourceAsset)
	if err != nil {
		return wire.Quote{}, err
	}
	destination, _, err := resolve(req.Destination.Asset)
	if err != nil {
		return wire.Quote{}, err
	}
	if !sameAsset(source, account.Public.Asset) {
		return wire.Quote{}, problem(422, "INVALID_INPUT", "Source account asset differs")
	}
	if !validAddress(destination, req.Destination.Address) {
		return wire.Quote{}, problem(422, "INVALID_INPUT", "Invalid destination address")
	}
	principal := amount(req.Amount)
	if !principal.Equal(principal.Truncate(scale)) {
		return wire.Quote{}, problem(422, "INVALID_PRECISION", "Amount exceeds accounting scale")
	}
	if req.Bridge == nil && !sameAsset(source, destination) {
		return wire.Quote{}, problem(422, "UNSUPPORTED_QUOTE_COMBINATION", "Cross-network transfer requires a bridge selection")
	}
	if req.Bridge != nil {
		route := bridgeRoutes().Items[0]
		if req.Bridge.RouteRef != route.RouteRef || !sameAsset(source, route.SourceAsset) || !sameAsset(destination, route.DestinationAsset) {
			return wire.Quote{}, problem(422, "UNSUPPORTED_QUOTE_COMBINATION", "Unsupported bridge route")
		}
	}
	fee := decimal.New(1, -2)
	recipient, total := principal, principal.Add(fee)
	if req.SettlementMode == "FIX_RECEIVED" {
		if req.FeeTreatment == "DEDUCT_FROM_DESTINATION" {
			principal = principal.Add(fee)
			total = principal
		}
	} else if req.FeeTreatment == "DEDUCT_FROM_DESTINATION" {
		total = principal
		recipient = principal.Sub(fee)
	}
	if !recipient.IsPositive() {
		return wire.Quote{}, problem(422, "UNSUPPORTED_QUOTE_COMBINATION", "Fees exceed destination amount")
	}
	now := s.stamp()
	expires := s.Now().Add(time.Minute).Format(time.RFC3339Nano)
	fees := []wire.Fee{{Code: "network", Amount: wire.Money{Amount: fee.String(), Currency: source.Currency}, Asset: source}}
	q := wire.Quote{
		QuoteRef:        ref,
		State:           "EXECUTABLE",
		Readiness:       "LIQUIDITY_READY",
		FeeTreatment:    req.FeeTreatment,
		SettlementMode:  req.SettlementMode,
		Principal:       &wire.Money{Amount: principal.String(), Currency: source.Currency},
		PayerTotalDebit: &wire.Money{Amount: total.String(), Currency: source.Currency},
		RecipientAmount: &wire.Money{Amount: recipient.String(), Currency: destination.Currency},
		Fees:            &fees,
		PricedAt:        &now,
		ExpiresAt:       &expires,
		Instructions:    wire.TransferInstructions{SourceAccountRef: req.SourceAccountRef, SourceAsset: source, Destination: req.Destination, Bridge: req.Bridge},
	}
	q.Instructions.Destination.Asset = destination
	s.Quotes[ref] = &Quote{Public: q, Tenant: t.Ref, Actor: t.Actor}
	return q, nil
}
func (s *State) receive(t *Tenant, ref string, input []byte) (wire.ReceivingAddress, error) {
	var req wire.CreateReceivingAddress
	_ = json.Unmarshal(input, &req)
	a, err := s.authorized(t, req.AccountRef)
	if err != nil {
		return wire.ReceivingAddress{}, err
	}
	asset, _, err := resolve(req.Asset)
	if err != nil {
		return wire.ReceivingAddress{}, err
	}
	if !sameAsset(asset, a.Public.Asset) {
		return wire.ReceivingAddress{}, problem(422, "INVALID_INPUT", "Receiving account asset differs")
	}
	address := syntheticAddress(ref, asset)
	out := wire.ReceivingAddress{ReceivingAddressRef: ref, AccountRef: req.AccountRef, Asset: asset, Address: address, Status: "ACTIVE", Version: "1", CreatedAt: ptr(s.stamp())}
	s.put(Record{Tenant: t.Ref, Surface: "receiving-addresses", Ref: ref, Account: req.AccountRef, Body: raw(out)})
	return out, nil
}
