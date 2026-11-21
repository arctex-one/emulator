package sim

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	wire "github.com/arctex-one/client-go/generated"
	_ "modernc.org/sqlite"
)

type Tenant struct {
	Ref        string   `json:"tenant_ref"`
	Actor      string   `json:"actor_ref"`
	Bearer     string   `json:"bearer"`
	Accounts   []string `json:"accounts"`
	Callback   Callback `json:"callback"`
	SigningKey []byte
}
type Callback struct {
	URL       string `json:"url"`
	Algorithm string `json:"algorithm"`
	Secret    string `json:"secret"`
}
type Account struct {
	Public  wire.Account
	Tenant  string
	Balance string
	Version uint64
	Updated string
}
type Quote struct {
	Public                  wire.Quote
	Tenant, Actor, Transfer string
}
type Transfer struct {
	Public                     wire.Transfer
	Tenant                     string
	Scenario                   string
	Phase                      string
	Reserved                   bool
	PrincipalEffect, FeeEffect string
	Refund                     *wire.TransferAccountingRefund
}
type Record struct {
	Sequence                                                                           int64
	Tenant, Surface, Ref, Account, Quote, State, Receiving, Deposit, Transfer, Journal string
	Body                                                                               json.RawMessage
}
type Command struct {
	Input, Account string
	Status         int
	Body           json.RawMessage
	Retired        bool
}
type Event struct {
	ID, Tenant, Type, Occurred string
	Data                       json.RawMessage
	Attempts                   int
	Status                     string
	Next                       time.Time
	LastStatus                 int
	LastError                  string
	LastDelivery               string
	Callback                   Callback
}
type Scenario struct {
	Outcome string `json:"outcome"`
}
type State struct {
	Sequence      int64
	Manual        bool
	Clock         *time.Time
	CursorKey     []byte
	Tenants       []Tenant
	Accounts      map[string]*Account
	Quotes        map[string]*Quote
	Transfers     map[string]*Transfer
	Commands      map[string]Command
	Scenarios     map[string]Scenario
	Deposits      map[string]wire.Deposit
	DepositInputs map[string]string
	Records       []Record
	Events        []*Event
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS simulator (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, state BLOB NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	var exists int
	err = db.QueryRow("SELECT count(*) FROM simulator").Scan(&exists)
	if err == nil && exists == 0 {
		var state *State
		state, err = seed()
		if err == nil {
			var raw []byte
			raw, err = json.Marshal(state)
			if err == nil {
				_, err = db.Exec("INSERT INTO simulator VALUES(1,1,?)", raw)
			}
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	// A delivery may have reached its receiver before shutdown. Replaying its stable
	// event identity is safe for a consumer's durable inbox.
	err = s.Update(context.Background(), func(st *State) error {
		for _, e := range st.Events {
			if e.Status == "sending" {
				e.Status = "pending"
				e.Next = st.Now()
			}
		}
		if !st.Manual {
			for _, ref := range sortedTransferRefs(st) {
				t := st.Transfers[ref]
				if t.Phase != "done" && t.Scenario == "success" {
					if err := st.run(t, true); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Update(ctx context.Context, fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT version,state FROM simulator WHERE id=1").Scan(&version, &raw); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported emulator state version %d", version)
	}
	var st State
	if err = json.Unmarshal(raw, &st); err != nil {
		return err
	}
	if err = fn(&st); err != nil {
		return err
	}
	raw, err = json.Marshal(&st)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE simulator SET state=? WHERE id=1", raw); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *State) Now() time.Time {
	if s.Clock != nil {
		return s.Clock.UTC()
	}
	return time.Now().UTC()
}
func (s *State) stamp() string { return s.Now().Format(time.RFC3339Nano) }
func (s *State) id(prefix string) string {
	s.Sequence++
	return fmt.Sprintf("emu_%s_%012d", prefix, s.Sequence)
}
func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func ptr[T any](v T) *T { return &v }
func (s *State) tenant(ref string) *Tenant {
	for i := range s.Tenants {
		if s.Tenants[i].Ref == ref {
			return &s.Tenants[i]
		}
	}
	return nil
}
func (s *State) authorized(t *Tenant, ref string) (*Account, error) {
	a := s.Accounts[ref]
	if a == nil || a.Tenant != t.Ref {
		return nil, notFound()
	}
	for _, r := range t.Accounts {
		if ref == r {
			return a, nil
		}
	}
	return nil, notFound()
}
func (s *State) put(r Record) { r.Sequence = s.next(); s.Records = append(s.Records, r) }
func (s *State) next() int64  { s.Sequence++; return s.Sequence }
func (s *State) event(tenant, kind string, data any) {
	t := s.tenant(tenant)
	s.Events = append(s.Events, &Event{ID: s.id("event"), Tenant: tenant, Type: kind, Occurred: s.stamp(), Data: raw(data), Status: "pending", Next: s.Now(), Callback: t.Callback})
}
func seed() (*State, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	st := &State{
		CursorKey:     key,
		Accounts:      map[string]*Account{},
		Quotes:        map[string]*Quote{},
		Transfers:     map[string]*Transfer{},
		Commands:      map[string]Command{},
		Scenarios:     map[string]Scenario{},
		Deposits:      map[string]wire.Deposit{},
		DepositInputs: map[string]string{},
		Records:       []Record{},
		Events:        []*Event{},
	}
	for _, name := range []string{"demo", "other"} {
		_, signing, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		t := Tenant{Ref: name, Actor: name, Bearer: "emulator-" + name, SigningKey: signing, Callback: Callback{Algorithm: "hmac-sha256", Secret: "emulator-" + name + "-callback"}}
		st.Tenants = append(st.Tenants, t)
		for _, item := range catalog().Items {
			ref := "acct_" + name + "_" + item.Asset.Network + "_" + item.Asset.Currency
			st.Accounts[ref] = &Account{Public: wire.Account{AccountRef: ref, Asset: item.Asset, Status: "active"}, Tenant: name, Balance: "0"}
			st.Tenants[len(st.Tenants)-1].Accounts = append(st.Tenants[len(st.Tenants)-1].Accounts, ref)
			st.post(name, ref, "OPENING", []posting{{Amount: "1000000", Kind: "MAIN"}}, raw(wire.AdjustmentOrigin{Type: "ADJUSTMENT", ReasonCode: "emulator_opening"}), st.stamp())
		}
	}
	// Opening fixtures are discoverable via effects/statements without sending
	// notifications before a development callback has been configured.
	st.Events = []*Event{}
	return st, nil
}

var errInvalidState = errors.New("invalid emulator state")
