package sim

import (
	"encoding/json"
	wire "github.com/arctex-one/client-go/generated"
	"github.com/shopspring/decimal"
	"strconv"
	"time"
)

type posting struct{ Amount, Kind, Compensates string }

func amount(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// post is the sole writer of balances, statements and immutable financial effects.
// The enclosing SQLite transaction also commits each resulting webhook event.
func (s *State) post(tenant, account, kind string, lines []posting, origin json.RawMessage, eventTime string) (string, []string) {
	a := s.Accounts[account]
	journal := s.id("journal")
	a.Version++
	entry := wire.StatementEntry{
		EntryRef:      journal,
		EntryType:     kind,
		AccountAmount: wire.Money{Amount: "0", Currency: a.Public.Asset.Currency},
		Asset:         a.Public.Asset,
		EventTime:     eventTime,
		Version:       strconv.FormatUint(a.Version, 10),
		Lines:         []wire.PostingLine{},
		EffectRefs:    []string{},
	}
	var refs struct {
		Deposit  string `json:"deposit_ref"`
		Transfer string `json:"transfer_ref"`
	}
	_ = json.Unmarshal(origin, &refs)
	for _, line := range lines {
		if amount(line.Amount).IsZero() {
			continue
		}
		effectRef := s.id("effect")
		effect := wire.FinancialEffect{
			EffectRef:       effectRef,
			JournalEntryRef: journal,
			PostingLineRef:  effectRef,
			AccountRef:      account,
			Money:           wire.Money{Amount: line.Amount, Currency: a.Public.Asset.Currency},
			Asset:           a.Public.Asset,
			LineType:        line.Kind,
			Origin:          origin,
			CommittedAt:     s.stamp(),
		}
		if line.Compensates != "" {
			effect.CompensatesEffectRef = ptr(line.Compensates)
		}
		a.Balance = amount(a.Balance).Add(amount(line.Amount)).String()
		entry.AccountAmount.Amount = amount(entry.AccountAmount.Amount).Add(amount(line.Amount)).String()
		entry.EffectRefs = append(entry.EffectRefs, effectRef)
		entry.Lines = append(entry.Lines, wire.PostingLine{AccountRef: account, Money: effect.Money, LineType: line.Kind, PostingLineRef: effectRef, EffectRef: effectRef, Origin: origin, Asset: a.Public.Asset})
		s.put(Record{Tenant: tenant, Surface: "financial-effects", Ref: effectRef, Account: account, Deposit: refs.Deposit, Transfer: refs.Transfer, Journal: journal, Body: raw(effect)})
		s.event(tenant, "financial_effect.committed", effect)
	}
	a.Updated = s.stamp()
	s.put(Record{Tenant: tenant, Surface: "statements", Ref: journal, Account: account, Journal: journal, Body: raw(entry)})
	return journal, entry.EffectRefs
}
func (s *State) balance(a *Account, at *time.Time) wire.Balance {
	b := wire.Balance{AccountRef: a.Public.AccountRef, Asset: a.Public.Asset, Balance: wire.Money{Amount: a.Balance, Currency: a.Public.Asset.Currency}, Version: ptr(strconv.FormatUint(a.Version, 10)), LastUpdated: ptr(a.Updated)}
	if at == nil {
		return b
	}
	b.Version = nil
	b.LastUpdated = nil
	b.AsOf = ptr(at.UTC().Format(time.RFC3339Nano))
	total := decimal.Zero
	for _, r := range s.Records {
		if r.Surface != "statements" || r.Account != a.Public.AccountRef {
			continue
		}
		var e wire.StatementEntry
		_ = json.Unmarshal(r.Body, &e)
		when, _ := time.Parse(time.RFC3339Nano, e.EventTime)
		if !when.After(*at) {
			total = total.Add(amount(e.AccountAmount.Amount))
		}
	}
	b.Balance.Amount = total.String()
	return b
}
