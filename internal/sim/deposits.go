package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	wire "github.com/arctex-one/client-go/generated"
	"time"
)

type DepositInput struct {
	Tenant    string `json:"tenant_ref"`
	Receiving string `json:"receiving_address_ref"`
	Amount    string `json:"amount"`
	Event     string `json:"event_ref"`
	Pending   bool   `json:"pending"`
	EventTime string `json:"event_time,omitempty"`
}

func (s *State) inject(req DepositInput) (wire.Deposit, error) {
	if req.Event == "" || req.Tenant == "" {
		return wire.Deposit{}, problem(400, "INVALID_REQUEST", "tenant_ref and event_ref are required")
	}
	t := s.tenant(req.Tenant)
	if t == nil {
		return wire.Deposit{}, notFound()
	}
	canonical := string(raw(req))
	key := req.Tenant + "/" + req.Event
	if prior, ok := s.DepositInputs[key]; ok {
		if prior != canonical {
			return wire.Deposit{}, problem(409, "IDEMPOTENCY_CONFLICT", "Deposit identity already has different input")
		}
		return s.Deposits[key], nil
	}
	var address wire.ReceivingAddress
	for _, r := range s.Records {
		if r.Tenant == req.Tenant && r.Surface == "receiving-addresses" && r.Ref == req.Receiving {
			_ = json.Unmarshal(r.Body, &address)
		}
	}
	if address.ReceivingAddressRef == "" {
		return wire.Deposit{}, notFound()
	}
	if _, err := s.authorized(t, address.AccountRef); err != nil {
		return wire.Deposit{}, err
	}
	value, err := positiveAmount(req.Amount, address.Asset)
	if err != nil {
		return wire.Deposit{}, err
	}
	if req.EventTime != "" {
		if _, err = time.Parse(time.RFC3339Nano, req.EventTime); err != nil {
			return wire.Deposit{}, problem(400, "INVALID_REQUEST", "Invalid event_time")
		}
	}
	now := s.stamp()
	dep := wire.Deposit{
		DepositRef:    s.id("deposit"),
		InboundRef:    req.Event,
		AccountRef:    ptr(address.AccountRef),
		Amount:        value,
		Asset:         address.Asset,
		State:         "OBSERVED",
		Version:       "1",
		CreatedAt:     now,
		UpdatedAt:     now,
		Receiving:     wire.ReceivingAttribution{Address: address.Address, Memo: address.Memo, CapturedAt: now, ReceivingAddressRef: ptr(address.ReceivingAddressRef), RouteVersion: ptr(address.Version)},
		ChainEvidence: &wire.ChainEvidence{Network: address.Asset.Network, TransactionHash: chainHash(req.Event, address.Asset.Network), TransferIdentity: ptr(req.Event), Finality: ptr("OBSERVED")},
	}
	s.Deposits[key] = dep
	s.DepositInputs[key] = canonical
	s.saveDeposit(t.Ref, dep)
	if !req.Pending {
		s.completeDeposit(key, req.EventTime)
	}
	return s.Deposits[key], nil
}
func (s *State) completeDeposit(key, eventTime string) {
	dep := s.Deposits[key]
	if dep.State == "COMPLETED" {
		return
	}
	if eventTime == "" {
		eventTime = s.stamp()
	}
	tenant := s.Accounts[*dep.AccountRef].Tenant
	journal, _ := s.post(tenant, *dep.AccountRef, "DEPOSIT", []posting{{Amount: dep.Amount, Kind: "MAIN"}}, raw(wire.DepositOrigin{Type: "DEPOSIT", DepositRef: dep.DepositRef}), eventTime)
	dep.State = "COMPLETED"
	dep.Version = "2"
	dep.UpdatedAt = s.stamp()
	dep.CreditJournalEntryRef = ptr(journal)
	dep.ChainEvidence.Finality = ptr("FINALIZED")
	s.Deposits[key] = dep
	s.saveDeposit(tenant, dep)
}
func (s *State) saveDeposit(tenant string, dep wire.Deposit) {
	s.put(Record{Tenant: tenant, Surface: "deposits", Ref: dep.DepositRef, Account: *dep.AccountRef, Receiving: *dep.Receiving.ReceivingAddressRef, Deposit: dep.DepositRef, Body: raw(dep)})
	s.event(tenant, "deposit.updated", dep)
}
func positiveAmount(value string, asset wire.Asset) (string, error) {
	canonical, err := contractAmount(value)
	if err != nil {
		return "", err
	}
	_, scale, err := resolve(asset)
	if err != nil {
		return "", err
	}
	if !amount(canonical).Equal(amount(canonical).Truncate(scale)) {
		return "", problem(422, "INVALID_PRECISION", "Amount exceeds accounting scale")
	}
	return canonical, nil
}
func syntheticAddress(ref string, asset wire.Asset) string {
	sum := sha256.Sum256([]byte(ref))
	if asset.Network == "tron_mainnet" {
		// Base58Check encodes a synthetic public address with no corresponding key.
		payload := append([]byte{0x41}, sum[:20]...)
		first := sha256.Sum256(payload)
		second := sha256.Sum256(first[:])
		payload = append(payload, second[:4]...)
		return base58(payload)
	}
	return "0x" + hex.EncodeToString(sum[:20])
}
