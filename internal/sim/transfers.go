package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	wire "github.com/arctex-one/client-go/generated"
	"sort"
	"strconv"
	"time"
)

func sortedTransferRefs(s *State) []string {
	refs := make([]string, 0, len(s.Transfers))
	for ref := range s.Transfers {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}
func (s *State) submit(t *Tenant, ref string, input []byte) (wire.Transfer, error) {
	var req wire.SubmitTransfer
	_ = json.Unmarshal(input, &req)
	q := s.Quotes[req.QuoteRef]
	if q == nil || q.Tenant != t.Ref {
		return wire.Transfer{}, notFound()
	}
	if _, err := s.authorized(t, q.Public.Instructions.SourceAccountRef); err != nil {
		return wire.Transfer{}, err
	}
	if q.Actor != t.Actor && q.Transfer == "" {
		return wire.Transfer{}, problem(403, "PERMISSION_DENIED", "Quote belongs to another actor")
	}
	expires, _ := time.Parse(time.RFC3339Nano, *q.Public.ExpiresAt)
	if !s.Now().Before(expires) {
		return wire.Transfer{}, problem(409, "QUOTE_EXPIRED", "Recover any existing transfer by quote reference")
	}
	if q.Transfer != "" {
		return s.Transfers[q.Transfer].Public, nil
	}
	if q.Public.State != "EXECUTABLE" {
		return wire.Transfer{}, problem(409, "QUOTE_NOT_EXECUTABLE", "Quote is not executable")
	}
	scenario := "success"
	if v, ok := s.Scenarios[q.Public.QuoteRef]; ok {
		scenario = v.Outcome
	}
	out := wire.Transfer{
		TransferRef:     ref,
		QuoteRef:        q.Public.QuoteRef,
		Quote:           q.Public,
		ClientReference: req.ClientReference,
		State:           "CREATED",
		Version:         "1",
		CreatedAt:       s.stamp(),
		UpdatedAt:       s.stamp(),
		Executions:      ptr([]wire.ExecutionObservation{}),
		Bridges:         ptr([]wire.BridgeStatus{}),
	}
	tr := &Transfer{Public: out, Tenant: t.Ref, Scenario: scenario, Phase: "reserve"}
	q.Transfer = ref
	s.Transfers[ref] = tr
	s.saveTransfer(tr)
	// Keep the accepted response immutable for idempotent replay, even when the
	// default scenario completes before this request returns.
	accepted := out
	if !s.Manual {
		if err := s.run(tr, true); err != nil {
			return out, err
		}
	}
	return accepted, nil
}
func (s *State) saveTransfer(t *Transfer) {
	p := t.Public
	s.put(Record{Tenant: t.Tenant, Surface: "transfers", Ref: p.TransferRef, Account: p.Quote.Instructions.SourceAccountRef, Quote: p.QuoteRef, State: p.State, Transfer: p.TransferRef, Body: raw(p)})
	s.event(t.Tenant, "transfer.updated", p)
}
func (s *State) changed(t *Transfer) {
	n, _ := strconv.ParseUint(t.Public.Version, 10, 64)
	t.Public.Version = strconv.FormatUint(n+1, 10)
	t.Public.UpdatedAt = s.stamp()
	s.saveTransfer(t)
}
func origin(t *Transfer) json.RawMessage {
	return raw(wire.TransferOrigin{Type: "TRANSFER", TransferRef: t.Public.TransferRef, ClientReference: t.Public.ClientReference})
}
func (s *State) run(t *Transfer, all bool) error {
	for i := 0; i < 12; i++ {
		prior := t.Phase
		if err := s.step(t); err != nil {
			return err
		}
		if !all || t.Phase == prior || t.Phase == "done" {
			return nil
		}
	}
	return errInvalidState
}
func (s *State) step(t *Transfer) error {
	q := t.Public.Quote
	source := q.Instructions.SourceAccountRef
	total := amount(q.PayerTotalDebit.Amount)
	fee := amount((*q.Fees)[0].Amount.Amount)
	principal := total.Sub(fee)
	switch t.Phase {
	case "reserve":
		if amount(s.Accounts[source].Balance).LessThan(total) {
			t.Public.State = "FAILED"
			t.Public.Attention = &wire.Attention{ReasonCode: "INSUFFICIENT_FUNDS", Message: "Insufficient simulated financial availability", Action: "NONE"}
			t.Phase = "done"
			t.Public.CompletedAt = ptr(s.stamp())
			s.changed(t)
			return nil
		}
		_, effects := s.post(t.Tenant, source, "RESERVATION", []posting{{Amount: principal.Neg().String(), Kind: "MAIN"}, {Amount: fee.Neg().String(), Kind: "FEE"}}, origin(t), s.stamp())
		t.PrincipalEffect, t.FeeEffect = effects[0], effects[1]
		t.Reserved = true
		t.Public.State = "EXECUTING"
		t.Public.Executions = ptr([]wire.ExecutionObservation{{ExecutionRef: s.id("execution"), Network: q.Instructions.SourceAsset.Network, Status: "PROCESSING"}})
		t.Phase = "broadcast"
		s.changed(t)
	case "broadcast":
		if t.Scenario == "pending" {
			return nil
		}
		if t.Scenario == "fail_before_broadcast" {
			s.failTransfer(t, false)
			return nil
		}
		e := &(*t.Public.Executions)[0]
		e.Status = "BROADCASTED"
		e.TransactionHash = ptr(chainHash(e.ExecutionRef, e.Network))
		t.Phase = "finalize"
		s.changed(t)
	case "finalize":
		if t.Scenario == "pause_before_finality" {
			return nil
		}
		if t.Scenario == "fail_after_broadcast" {
			s.failTransfer(t, true)
			return nil
		}
		(*t.Public.Executions)[0].Status = "SUCCEEDED"
		if q.Instructions.Bridge != nil {
			t.Public.Bridges = ptr([]wire.BridgeStatus{{BridgeRef: s.id("bridge"), Stage: "ATTESTATION_WAIT", BurnTransactionRef: (*t.Public.Executions)[0].TransactionHash}})
			t.Phase = "mint"
			s.changed(t)
			return nil
		}
		s.settled(t)
	case "mint":
		if t.Scenario == "pause_before_mint" {
			return nil
		}
		dest := s.destinationAccount(t)
		if dest == "" {
			return errInvalidState
		}
		s.post(t.Tenant, dest, "BRIDGE_MINT", []posting{{Amount: q.RecipientAmount.Amount, Kind: "MAIN"}}, origin(t), s.stamp())
		bridge := &(*t.Public.Bridges)[0]
		bridge.Stage = "COMPLETED"
		bridge.MintTransactionRef = ptr(chainHash(bridge.BridgeRef, q.Instructions.Destination.Asset.Network))
		t.Public.Executions = ptr(append(*t.Public.Executions, wire.ExecutionObservation{ExecutionRef: s.id("execution"), Network: q.Instructions.Destination.Asset.Network, Status: "PROCESSING"}))
		t.Phase = "destination_payout"
		s.changed(t)
	case "destination_payout":
		dest := s.destinationAccount(t)
		s.post(t.Tenant, dest, "PAYOUT", []posting{{Amount: amount(q.RecipientAmount.Amount).Neg().String(), Kind: "MAIN"}}, origin(t), s.stamp())
		e := &(*t.Public.Executions)[1]
		e.Status = "SUCCEEDED"
		e.TransactionHash = ptr(chainHash(e.ExecutionRef, e.Network))
		s.settled(t)
	case "done":
		return nil
	default:
		return errInvalidState
	}
	return nil
}
func (s *State) destinationAccount(t *Transfer) string {
	for _, ref := range s.tenant(t.Tenant).Accounts {
		if sameAsset(s.Accounts[ref].Public.Asset, t.Public.Quote.Instructions.Destination.Asset) {
			return ref
		}
	}
	return ""
}
func (s *State) settled(t *Transfer) {
	t.Reserved = false
	t.Phase = "done"
	t.Public.State = "SETTLED"
	t.Public.Attention = nil
	t.Public.CompletedAt = ptr(s.stamp())
	s.changed(t)
}
func (s *State) failTransfer(t *Transfer, afterBroadcast bool) {
	q := t.Public.Quote
	total := amount(q.PayerTotalDebit.Amount)
	fee := amount((*q.Fees)[0].Amount.Amount)
	principal := total.Sub(fee)
	lines := []posting{{Amount: principal.String(), Kind: "MAIN", Compensates: t.PrincipalEffect}}
	if !afterBroadcast {
		lines = append(lines, posting{Amount: fee.String(), Kind: "FEE", Compensates: t.FeeEffect})
	}
	journal, _ := s.post(t.Tenant, q.Instructions.SourceAccountRef, "RELEASE", lines, origin(t), s.stamp())
	if !afterBroadcast && q.Instructions.Bridge == nil {
		t.Refund = &wire.TransferAccountingRefund{Released: *q.PayerTotalDebit, JournalRefs: []string{journal}}
	}
	(*t.Public.Executions)[0].Status = "FAILED"
	t.Public.State = "FAILED"
	t.Public.Attention = &wire.Attention{ReasonCode: "EXECUTION_FAILED", Message: "Configured simulated execution failure", Action: "NONE"}
	t.Public.CompletedAt = ptr(s.stamp())
	t.Phase = "done"
	t.Reserved = false
	s.changed(t)
}
func transactionHash(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])
}

func chainHash(ref, network string) string {
	value := transactionHash(ref)
	if network != "tron_mainnet" {
		return "0x" + value
	}
	return value
}
