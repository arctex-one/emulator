package sim

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	consumer "github.com/arctex-one/client-go"
	wire "github.com/arctex-one/client-go/generated"
	"github.com/arctex-one/emulator/internal/contract"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type fixture struct {
	t               *testing.T
	store           *Store
	api             *API
	public, control *httptest.Server
	path            string
	compiler        *jsonschema.Compiler
}
type positiveRegexp struct {
	*regexp.Regexp
	positive bool
	original string
}

func (r positiveRegexp) MatchString(s string) bool {
	return r.Regexp.MatchString(s) && (!r.positive || strings.ContainsAny(s, "123456789"))
}
func (r positiveRegexp) String() string { return r.original }
func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sim.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: store, path: path}
	f.api = New(store, "control")
	f.public = httptest.NewServer(f.api.Public())
	f.control = httptest.NewServer(f.api.Control())
	doc, err := jsonschema.UnmarshalJSON(contract.OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	f.compiler = jsonschema.NewCompiler()
	f.compiler.AssertFormat()
	f.compiler.UseRegexpEngine(func(pattern string) (jsonschema.Regexp, error) {
		re, err := regexp.Compile(strings.Replace(pattern, "(?=.*[1-9])", "", 1))
		return positiveRegexp{re, strings.Contains(pattern, "(?=.*[1-9])"), pattern}, err
	})
	if err = f.compiler.AddResource("https://contract.test/api.json", doc); err != nil {
		t.Fatal(err)
	}
	f.controlCall("PUT", "/control/settings", map[string]any{"manual": true, "now": "2030-01-01T00:00:00Z"}, 200)
	t.Cleanup(func() { f.public.Close(); f.control.Close(); f.store.Close() })
	return f
}
func (f *fixture) validate(body []byte, name string) {
	f.t.Helper()
	schema, err := f.compiler.Compile("https://contract.test/api.json#/components/schemas/" + name)
	if err != nil {
		f.t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err == nil {
		err = schema.Validate(value)
	}
	if err != nil {
		f.t.Fatalf("%s contract violation: %v\n%s", name, err, body)
	}
}
func (f *fixture) call(method, path, key string, body any, status int, schema string) []byte {
	f.t.Helper()
	return f.callAs("emulator-demo", method, path, key, body, status, schema)
}
func (f *fixture) callAs(bearer, method, path, key string, body any, status int, schema string) []byte {
	f.t.Helper()
	var payload []byte
	if body != nil {
		if v, ok := body.([]byte); ok {
			payload = v
		} else {
			payload = raw(body)
		}
	}
	req, _ := http.NewRequest(method, f.public.URL+path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := f.public.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		f.t.Fatalf("%s %s got %d, want %d: %s", method, path, response.StatusCode, status, data)
	}
	if response.Header.Get("X-Request-ID") == "" {
		f.t.Fatal("request identifier missing")
	}
	if status >= 400 {
		schema = "Error"
	}
	if schema != "" {
		f.validate(data, schema)
	}
	return data
}
func (f *fixture) controlCall(method, path string, body any, status int) []byte {
	f.t.Helper()
	var payload []byte
	if body != nil {
		payload = raw(body)
	}
	req, _ := http.NewRequest(method, f.control.URL+path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer control")
	response, err := f.control.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		f.t.Fatalf("control %s got %d want %d: %s", path, response.StatusCode, status, data)
	}
	return data
}
func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func quoteInput() wire.QuoteRequest {
	asset := catalog().Items[1].Asset
	return wire.QuoteRequest{
		SourceAccountRef: "acct_demo_tron_mainnet_USDT",
		SourceAsset:      asset,
		Destination:      wire.Destination{Asset: asset, Address: syntheticAddress("destination", asset)},
		Amount:           "100",
		SettlementMode:   "FIX_SOURCE",
		FeeTreatment:     "ADD_TO_SOURCE",
	}
}
func (f *fixture) quote(key string, req wire.QuoteRequest) wire.Quote {
	return decode[wire.Quote](f.t, f.call("POST", "/v1/quotes", key, req, 200, "Quote"))
}
func (f *fixture) submit(key string, q wire.Quote) wire.Transfer {
	return decode[wire.Transfer](f.t, f.call("POST", "/v1/transfers", key, wire.SubmitTransfer{QuoteRef: q.QuoteRef}, 202, "Transfer"))
}
func (f *fixture) transfer(ref string) wire.Transfer {
	return decode[wire.Transfer](f.t, f.call("GET", "/v1/transfers/"+ref, "", nil, 200, "Transfer"))
}
func (f *fixture) accountBalance(ref string) string {
	b := decode[wire.Balance](f.t, f.call("GET", "/v1/accounts/"+ref+"/balance", "", nil, 200, "Balance"))
	return b.Balance.Amount
}
func (f *fixture) resume(ref string) {
	f.controlCall("POST", "/control/transfers/"+ref+"/resume", nil, 200)
}
func (f *fixture) scenario(q wire.Quote, outcome string) {
	f.controlCall("PUT", "/control/scenarios/"+q.QuoteRef, Scenario{outcome}, 200)
}
func (f *fixture) step(ref string) {
	f.controlCall("POST", "/control/transfers/"+ref+"/step", nil, 200)
}
func (f *fixture) receiving() wire.ReceivingAddress {
	req := wire.CreateReceivingAddress{AccountRef: quoteInput().SourceAccountRef, Asset: quoteInput().SourceAsset}
	return decode[wire.ReceivingAddress](f.t, f.call("POST", "/v1/receiving-addresses", "receiving", req, 201, "ReceivingAddress"))
}

func TestAllEighteenOperationsAndReconciliation(t *testing.T) {
	f := newFixture(t)
	f.call("GET", "/v1/assets", "", nil, 200, "AssetList")
	f.call("GET", "/v1/accounts", "", nil, 200, "AccountList")
	f.call("GET", "/v1/bridge-routes", "", nil, 200, "BridgeRouteList")
	account := quoteInput().SourceAccountRef
	f.call("GET", "/v1/accounts/"+account+"/balance?as_of=2000-01-01T00:00:00Z", "", nil, 200, "Balance")
	addr := f.receiving()
	f.call("GET", "/v1/receiving-addresses/"+addr.ReceivingAddressRef, "", nil, 200, "ReceivingAddress")
	f.call("GET", "/v1/receiving-addresses?account_ref="+account, "", nil, 200, "ReceivingAddressList")
	dep := decode[wire.Deposit](t, f.controlCall("POST", "/control/deposits", DepositInput{Tenant: "demo", Receiving: addr.ReceivingAddressRef, Amount: "25", Event: "deposit-1"}, 200))
	f.validate(raw(dep), "Deposit")
	f.call("GET", "/v1/deposits/"+dep.DepositRef, "", nil, 200, "Deposit")
	f.call("GET", "/v1/deposits", "", nil, 200, "DepositList")
	q := f.quote("quote", quoteInput())
	tr := f.submit("transfer", q)
	f.resume(tr.TransferRef)
	if f.transfer(tr.TransferRef).State != "SETTLED" {
		t.Fatal("payout not settled")
	}
	f.call("GET", "/v1/transfers?quote_ref="+q.QuoteRef, "", nil, 200, "TransferList")
	if got := f.accountBalance(account); got != "999924.99" {
		t.Fatalf("balance %s", got)
	}
	statements := decode[wire.StatementList](t, f.call("GET", "/v1/accounts/"+account+"/statement-entries?limit=200&offset=0", "", nil, 200, "StatementList"))
	total := amount("0")
	effects := map[string]bool{}
	for _, entry := range statements.Items {
		detail := decode[wire.StatementEntry](t, f.call("GET", "/v1/accounts/"+account+"/statement-entries/"+entry.EntryRef, "", nil, 200, "StatementEntry"))
		total = total.Add(amount(detail.AccountAmount.Amount))
		for _, ref := range detail.EffectRefs {
			if effects[ref] {
				t.Fatal("effect repeated")
			}
			effects[ref] = true
			effect := decode[wire.FinancialEffect](t, f.call("GET", "/v1/financial-effects/"+ref, "", nil, 200, "FinancialEffect"))
			if effect.JournalEntryRef != detail.EntryRef || effect.AccountRef != account {
				t.Fatal("statement and effect disagree")
			}
		}
	}
	if total.String() != f.accountBalance(account) {
		t.Fatal("statement sum differs from balance")
	}
	f.call("GET", "/v1/financial-effects?account_ref="+account, "", nil, 200, "FinancialEffectList")
	var kid string
	f.store.Update(t.Context(), func(s *State) error {
		key := s.tenant("demo").SigningKey
		kid = consumer.CallbackKeyID(ed25519.PrivateKey(key).Public().(ed25519.PublicKey))
		return nil
	})
	f.call("GET", "/v1/webhook-verification-keys/"+kid, "", nil, 200, "WebhookVerificationKey")
	f.callAs("emulator-other", "GET", "/v1/webhook-verification-keys/"+kid, "", nil, 404, "")
}

func TestPayoutScenariosAndReplay(t *testing.T) {
	for _, scenario := range []string{"success", "fail_before_broadcast", "fail_after_broadcast", "pending", "pause_before_finality"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			q := f.quote("quote", quoteInput())
			f.scenario(q, scenario)
			tr := f.submit("same", q)
			accepted := f.call("POST", "/v1/transfers", "same", wire.SubmitTransfer{QuoteRef: q.QuoteRef}, 202, "Transfer")
			if decode[wire.Transfer](t, accepted).TransferRef != tr.TransferRef {
				t.Fatal("duplicate transfer")
			}
			f.step(tr.TransferRef)
			f.step(tr.TransferRef)
			f.step(tr.TransferRef)
			got := f.transfer(tr.TransferRef)
			switch scenario {
			case "success":
				if got.State != "SETTLED" {
					t.Fatal(got.State)
				}
			case "fail_before_broadcast":
				if got.State != "FAILED" || got.AccountingRefund == nil || f.accountBalance(quoteInput().SourceAccountRef) != "1000000" {
					t.Fatal("missing full release")
				}
				list := decode[wire.TransferList](t, f.call("GET", "/v1/transfers", "", nil, 200, "TransferList"))
				if list.Items[0].AccountingRefund != nil {
					t.Fatal("detail refund leaked into retained list")
				}
			case "fail_after_broadcast":
				if got.State != "FAILED" || got.AccountingRefund != nil || f.accountBalance(quoteInput().SourceAccountRef) != "999999.99" {
					t.Fatal("partial release wrongly authorizes full refund")
				}
			default:
				if got.State != "EXECUTING" || f.accountBalance(quoteInput().SourceAccountRef) != "999899.99" {
					t.Fatal("pending reservation missing")
				}
				f.resume(tr.TransferRef)
			}
			before := f.accountBalance(quoteInput().SourceAccountRef)
			f.resume(tr.TransferRef)
			if f.accountBalance(quoteInput().SourceAccountRef) != before {
				t.Fatal("terminal replay changed finances")
			}
		})
	}
}

func TestContractValidationAndQuoteModes(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []string{"FIX_SOURCE", "FIX_RECEIVED"} {
		for _, treatment := range []string{"ADD_TO_SOURCE", "DEDUCT_FROM_DESTINATION"} {
			req := quoteInput()
			req.SettlementMode = mode
			req.FeeTreatment = treatment
			q := f.quote(mode+treatment, req)
			if amount(q.PayerTotalDebit.Amount).Sub(amount(q.RecipientAmount.Amount)).String() != "0.01" {
				t.Fatal("quote economics disagree")
			}
		}
	}
	f.call("POST", "/v1/quotes", "missing", []byte(`{}`), 400, "")
	f.call("POST", "/v1/quotes", "duplicate", []byte(`{"amount":"1","amount":"2"}`), 400, "")
	req := quoteInput()
	req.Amount = "1.0000001"
	f.call("POST", "/v1/quotes", "precision", req, 422, "")
	req = quoteInput()
	req.SourceAsset.ContractAddress = ptr("wrong")
	f.call("POST", "/v1/quotes", "asset", req, 422, "")
	req = quoteInput()
	req.Destination.Address = "invalid"
	f.call("POST", "/v1/quotes", "address", req, 422, "")
	req = quoteInput()
	req.Amount = "0"
	f.call("POST", "/v1/quotes", "zero", req, 400, "")
	f.call("POST", "/v1/quotes", "", quoteInput(), 400, "")
	f.call("GET", "/v1/transfers?limit=0", "", nil, 400, "")
	f.call("GET", "/v1/transfers?limit=1&limit=2", "", nil, 400, "")
	f.call("GET", "/v1/transfers?unknown=1", "", nil, 400, "")
	f.callAs("bad", "GET", "/v1/assets", "", nil, 401, "")
	for _, path := range []string{"/v1/accounts/acct_other_tron_mainnet_USDT/balance", "/v1/financial-effects?account_ref=acct_other_tron_mainnet_USDT"} {
		f.call("GET", path, "", nil, 404, "")
	}
}

func TestIdempotencyPresenceQuoteConvergenceAndExpiry(t *testing.T) {
	f := newFixture(t)
	req := quoteInput()
	req.SourceAsset.ContractAddress = nil
	q := f.quote("stable", req)
	req.SourceAsset = catalog().Items[1].Asset
	f.call("POST", "/v1/quotes", "stable", req, 409, "")
	tr := f.submit("first", q)
	other := f.submit("second", q)
	if other.TransferRef != tr.TransferRef {
		t.Fatal("quote admitted twice")
	}
	f.controlCall("PUT", "/control/settings", map[string]string{"advance": "2m"}, 200)
	if f.submit("second", q).TransferRef != tr.TransferRef {
		t.Fatal("same key expired")
	}
	f.call("POST", "/v1/transfers", "unseen", wire.SubmitTransfer{QuoteRef: q.QuoteRef}, 409, "")
	list := decode[wire.TransferList](t, f.call("GET", "/v1/transfers?quote_ref="+q.QuoteRef, "", nil, 200, "TransferList"))
	if len(list.Items) != 1 {
		t.Fatal("quote recovery lost")
	}
	f.controlCall("POST", "/control/idempotency/retire", map[string]string{"tenant_ref": "demo", "surface": "transfers", "key": "second"}, 200)
	f.call("POST", "/v1/transfers", "second", wire.SubmitTransfer{QuoteRef: q.QuoteRef}, 410, "")
}

func TestConcurrentClaimsAndInsufficientFunds(t *testing.T) {
	f := newFixture(t)
	q := f.quote("quote", quoteInput())
	var wg sync.WaitGroup
	refs := make(chan string, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			client := wire.Client{BaseURL: f.public.URL, Bearer: "emulator-demo", HTTP: f.public.Client()}
			tr, err := client.SubmitTransfer(context.Background(), nil, "concurrent-"+time.Duration(i).String(), wire.SubmitTransfer{QuoteRef: q.QuoteRef})
			if err != nil {
				errs <- err
				return
			}
			refs <- tr.TransferRef
		})
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	first := ""
	for ref := range refs {
		if first == "" {
			first = ref
		}
		if ref != first {
			t.Fatal("concurrent quote admitted multiple transfers")
		}
	}
	f.resume(first)
	if f.accountBalance(quoteInput().SourceAccountRef) != "999899.99" {
		t.Fatal("concurrent reservation duplicated")
	}
	req := quoteInput()
	req.Amount = "2000000"
	large := f.quote("large", req)
	tr := f.submit("large-transfer", large)
	f.step(tr.TransferRef)
	failed := f.transfer(tr.TransferRef)
	if failed.State != "FAILED" || failed.Attention.ReasonCode != "INSUFFICIENT_FUNDS" || failed.AccountingRefund != nil {
		t.Fatal("admission/financial failure semantics differ")
	}
}

func TestBridgePauseAndResume(t *testing.T) {
	f := newFixture(t)
	req := quoteInput()
	route := bridgeRoutes().Items[0]
	req.SourceAsset = route.SourceAsset
	req.SourceAccountRef = "acct_demo_monad_testnet_USDC"
	req.Destination.Asset = route.DestinationAsset
	req.Destination.Address = syntheticAddress("bridge-destination", route.DestinationAsset)
	req.Bridge = &wire.BridgeSelection{RouteRef: route.RouteRef, Mode: "STANDARD"}
	q := f.quote("bridge", req)
	f.scenario(q, "pause_before_mint")
	tr := f.submit("bridge", q)
	for i := 0; i < 5; i++ {
		f.step(tr.TransferRef)
	}
	pending := f.transfer(tr.TransferRef)
	if pending.State != "EXECUTING" || len(*pending.Bridges) != 1 || (*pending.Bridges)[0].MintTransactionRef != nil {
		t.Fatal("bridge minted while paused")
	}
	f.restart()
	if f.transfer(tr.TransferRef).State != "EXECUTING" {
		t.Fatal("bridge pause lost on restart")
	}
	dest := "acct_demo_arbitrum_sepolia_USDC"
	if f.accountBalance(dest) != "1000000" {
		t.Fatal("destination credited before mint")
	}
	f.resume(tr.TransferRef)
	if f.transfer(tr.TransferRef).State != "SETTLED" || f.accountBalance(dest) != "1000000" || f.accountBalance(req.SourceAccountRef) != "999899.99" {
		t.Fatal("bridge reconciliation differs")
	}
}

func TestRecoveryCursorsAndDelayedDepositCredit(t *testing.T) {
	f := newFixture(t)
	addr := f.receiving()
	input := DepositInput{Tenant: "demo", Receiving: addr.ReceivingAddressRef, Amount: "3", Event: "one", Pending: true}
	dep := decode[wire.Deposit](t, f.controlCall("POST", "/control/deposits", input, 200))
	history := decode[wire.DepositList](t, f.call("GET", "/v1/deposits?limit=1", "", nil, 200, "DepositList"))
	if len(history.Items) != 1 || history.HasMore {
		t.Fatal("initial revision missing")
	}
	cursor := history.NextCursor
	f.callAs("emulator-other", "GET", "/v1/deposits?cursor="+url.QueryEscape(cursor), "", nil, 400, "")
	f.call("GET", "/v1/financial-effects?cursor="+url.QueryEscape(cursor), "", nil, 400, "")
	f.call("GET", "/v1/deposits?account_ref="+addr.AccountRef+"&cursor="+url.QueryEscape(cursor), "", nil, 400, "")
	empty := decode[wire.DepositList](t, f.call("GET", "/v1/deposits?cursor="+url.QueryEscape(cursor), "", nil, 200, "DepositList"))
	if len(empty.Items) != 0 || empty.NextCursor == "" {
		t.Fatal("empty recovery checkpoint lost")
	}
	again := decode[wire.Deposit](t, f.controlCall("POST", "/control/deposits", input, 200))
	if again.DepositRef != dep.DepositRef {
		t.Fatal("deposit event duplicated")
	}
	input.Amount = "4"
	f.controlCall("POST", "/control/deposits", input, 409)
	f.controlCall("POST", "/control/deposits/"+dep.DepositRef+"/resume", nil, 200)
	f.controlCall("POST", "/control/deposits/"+dep.DepositRef+"/resume", nil, 200)
	revisions := decode[wire.DepositList](t, f.call("GET", "/v1/deposits?cursor="+url.QueryEscape(empty.NextCursor), "", nil, 200, "DepositList"))
	if len(revisions.Items) != 1 || revisions.Items[0].State != "COMPLETED" || revisions.Items[0].DepositRef != dep.DepositRef {
		t.Fatal("late credit revision lost")
	}
	if f.accountBalance(addr.AccountRef) != "1000003" {
		t.Fatal("duplicate deposit credited")
	}
	f.controlCall("PUT", "/control/settings", map[string]string{"advance": "24h"}, 200)
	f.call("GET", "/v1/deposits?cursor="+url.QueryEscape(cursor), "", nil, 410, "")
	f.call("GET", "/v1/deposits?cursor=bad", "", nil, 400, "")
	body, sig, _ := strings.Cut(cursor, ".")
	replacement := "A"
	if strings.HasPrefix(sig, "A") {
		replacement = "B"
	}
	tampered := body + "." + replacement + sig[1:]
	f.call("GET", "/v1/deposits?cursor="+url.QueryEscape(tampered), "", nil, 410, "")
}

func TestFinancialMutationRollsBackWithItsNotifications(t *testing.T) {
	f := newFixture(t)
	account := quoteInput().SourceAccountRef
	before := f.accountBalance(account)
	count := 0
	f.store.Update(t.Context(), func(s *State) error { count = len(s.Events); return nil })
	err := f.store.Update(t.Context(), func(s *State) error {
		s.post("demo", account, "ADJUSTMENT", []posting{{Amount: "123", Kind: "MAIN"}}, raw(wire.AdjustmentOrigin{Type: "ADJUSTMENT", ReasonCode: "rollback_test"}), s.stamp())
		return errInvalidState
	})
	if err != errInvalidState {
		t.Fatal("transaction did not fail")
	}
	if f.accountBalance(account) != before {
		t.Fatal("failed transaction changed balance")
	}
	f.store.Update(t.Context(), func(s *State) error {
		if len(s.Events) != count {
			t.Fatal("rolled-back financial event survived")
		}
		return nil
	})
}

func TestScenarioCanBeSelectedAfterAdapterAdmission(t *testing.T) {
	f := newFixture(t)
	q := f.quote("q", quoteInput())
	tr := f.submit("t", q)
	f.controlCall("PUT", "/control/scenarios/"+tr.TransferRef, Scenario{"fail_after_broadcast"}, 200)
	for i := 0; i < 3; i++ {
		f.step(tr.TransferRef)
	}
	if f.transfer(tr.TransferRef).State != "FAILED" {
		t.Fatal("admitted operation scenario not applied")
	}
	f.controlCall("PUT", "/control/scenarios/"+tr.TransferRef, Scenario{"success"}, 409)
}

func TestBackdatedCreditsRemainDiscoverableAndHistoricalBalancesReconcile(t *testing.T) {
	f := newFixture(t)
	addr := f.receiving()
	page := decode[wire.FinancialEffectList](t, f.call("GET", "/v1/financial-effects?account_ref="+addr.AccountRef, "", nil, 200, "FinancialEffectList"))
	f.controlCall("POST", "/control/deposits", DepositInput{Tenant: "demo", Receiving: addr.ReceivingAddressRef, Amount: "7", Event: "backdated", EventTime: "2001-01-01T00:00:00Z"}, 200)
	next := decode[wire.FinancialEffectList](t, f.call("GET", "/v1/financial-effects?account_ref="+addr.AccountRef+"&cursor="+url.QueryEscape(page.NextCursor), "", nil, 200, "FinancialEffectList"))
	if len(next.Items) != 1 || next.Items[0].Money.Amount != "7" {
		t.Fatal("late committed effect was skipped")
	}
	historical := decode[wire.Balance](t, f.call("GET", "/v1/accounts/"+addr.AccountRef+"/balance?as_of=2002-01-01T00:00:00Z", "", nil, 200, "Balance"))
	if historical.Balance.Amount != "7" || historical.Version != nil || historical.LastUpdated != nil {
		t.Fatal("event-time cutoff semantics differ")
	}
}

func (f *fixture) assertReconciled() {
	f.t.Helper()
	err := f.store.Update(context.Background(), func(s *State) error {
		sums := map[string]string{}
		statementSums := map[string]string{}
		seen := map[string]bool{}
		effects := map[string]wire.FinancialEffect{}
		references := map[string]bool{}
		for _, r := range s.Records {
			if r.Surface == "financial-effects" {
				var effect wire.FinancialEffect
				json.Unmarshal(r.Body, &effect)
				if seen[effect.EffectRef] {
					f.t.Errorf("duplicate immutable effect %s", effect.EffectRef)
				}
				seen[effect.EffectRef] = true
				effects[effect.EffectRef] = effect
				sum := sums[effect.AccountRef]
				if sum == "" {
					sum = "0"
				}
				sums[effect.AccountRef] = amount(sum).Add(amount(effect.Money.Amount)).String()
			}
			if r.Surface == "statements" {
				var e wire.StatementEntry
				json.Unmarshal(r.Body, &e)
				sum := statementSums[r.Account]
				if sum == "" {
					sum = "0"
				}
				statementSums[r.Account] = amount(sum).Add(amount(e.AccountAmount.Amount)).String()
				for _, ref := range e.EffectRefs {
					references[ref] = true
				}
			}
		}
		for ref, a := range s.Accounts {
			if sums[ref] != a.Balance || statementSums[ref] != a.Balance {
				f.t.Errorf("account %s differs from effect/statement sums", ref)
			}
		}
		if len(effects) != len(references) {
			f.t.Error("statement/effect membership differs")
		}
		for _, e := range s.Events {
			if e.Type != "financial_effect.committed" {
				continue
			}
			var effect wire.FinancialEffect
			json.Unmarshal(e.Data, &effect)
			saved, ok := effects[effect.EffectRef]
			if !ok || string(raw(saved)) != string(e.Data) {
				f.t.Error("event differs from immutable financial effect")
			}
		}
		return nil
	})
	if err != nil {
		f.t.Error(err)
	}
}

func TestControlAuthorityIsSeparateFromPublicAuthority(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("GET", f.control.URL+"/control/state", nil)
	req.Header.Set("Authorization", "Bearer emulator-demo")
	response, err := f.control.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("public bearer authorized simulator controls")
	}
	f.call("GET", "/control/state", "", nil, 404, "")
	f.callAs("control", "GET", "/v1/accounts", "", nil, 401, "")
}
