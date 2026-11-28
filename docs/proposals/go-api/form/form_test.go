package form_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
	"github.com/alepar/super-workflow/docs/proposals/go-api/form/formtest"
)

// Synthetic grammars: one minted id and one vocabulary.

type itemID struct{}

func (itemID) Name() string           { return "test-item-id" }
func (g itemID) Check(s string) error { return form.CheckMinted(g.Prefix(), s) }
func (itemID) Samples() []string      { return []string{"Q1", "Q12", "Q100"} }
func (itemID) Prefix() byte           { return 'Q' }
func (g itemID) Pattern() string      { return form.MintedPattern(g.Prefix()) }

var levelWords = []string{"low", "high"}

type level struct{}

func (level) Name() string           { return "test-level" }
func (g level) Check(s string) error { return form.CheckWord(s, g.Values()...) }
func (level) Samples() []string      { return levelWords }
func (level) Values() []string       { return levelWords }

type unregistered struct{}

func (unregistered) Name() string         { return "test-unregistered" }
func (unregistered) Check(s string) error { return nil }
func (unregistered) Samples() []string    { return []string{"x"} }

func init() {
	form.Register[itemID]()
	form.Register[level]()
}

func TestCorpus(t *testing.T) { formtest.CorpusTest(t) }

func TestParseMinted(t *testing.T) {
	for _, s := range []string{"Q0", "Q01", "q1", "F1", "Q", "Q1a", " Q1", "Q1\n", "Q99999999999999999999999"} {
		if v, err := form.Parse[itemID](s); err == nil {
			t.Errorf("Parse(%q) accepted as %q", s, v)
		} else if !v.IsZero() {
			t.Errorf("Parse(%q) refused but returned %q", s, v)
		}
	}
	v, err := form.Parse[itemID]("Q7")
	if err != nil || v.String() != "Q7" {
		t.Fatalf("Parse(Q7) = %q, %v", v, err)
	}
}

func TestAbsentIsNotEmpty(t *testing.T) {
	_, err := form.Parse[level]("")
	if !errors.Is(err, form.ErrEmpty) {
		t.Fatalf("Parse(\"\") = %v, want ErrEmpty", err)
	}
	var zero form.Of[level]
	if !zero.IsZero() {
		t.Fatal("zero value is not absent")
	}
	if _, err := zero.MarshalText(); err == nil {
		t.Fatal("absent value marshalled to text")
	}
}

func TestUnregisteredPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "not registered") {
			t.Fatalf("recover() = %v, want a not-registered panic", r)
		}
	}()
	form.Parse[unregistered]("x")
}

func TestMint(t *testing.T) {
	if got := form.Mint[itemID](nil).String(); got != "Q1" {
		t.Fatalf("Mint(nil) = %q", got)
	}
	recorded := []form.Of[itemID]{form.MustParse[itemID]("Q3"), {}, form.MustParse[itemID]("Q10")}
	if got := form.Mint(recorded).String(); got != "Q11" {
		t.Fatalf("Mint = %q, want Q11", got)
	}
}

// A JSON decode of a struct holding Of fields validates them; null and a
// missing field are absent; omitzero keeps an absent field out.
func TestJSON(t *testing.T) {
	type out struct {
		ID     form.Of[itemID]   `json:"id"`
		Level  form.Of[level]    `json:"level,omitzero"`
		Refs   []form.Of[itemID] `json:"refs"`
		ByID   map[form.Of[itemID]]string
		Absent form.Of[level] `json:"absent"`
	}
	var o out
	err := json.Unmarshal([]byte(`{"id":"Q2","refs":["Q1","Q3"],"ByID":{"Q4":"x"},"absent":null}`), &o)
	if err != nil {
		t.Fatal(err)
	}
	if o.ID.String() != "Q2" || len(o.Refs) != 2 || o.ByID[form.MustParse[itemID]("Q4")] != "x" || !o.Level.IsZero() || !o.Absent.IsZero() {
		t.Fatalf("decoded %+v", o)
	}
	for _, bad := range []string{`{"id":"Q0"}`, `{"id":""}`, `{"refs":["Q1","q2"]}`, `{"level":"medium"}`, `{"ByID":{"x":"y"}}`} {
		var o out
		if err := json.Unmarshal([]byte(bad), &o); err == nil {
			t.Errorf("decoded %s", bad)
		}
	}
	o.Absent = form.MustParse[level]("low")
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"Q2","refs":["Q1","Q3"],"ByID":{"Q4":"x"},"absent":"low"}`; string(b) != want {
		t.Fatalf("marshalled %s, want %s", b, want)
	}
	o.Absent = form.Of[level]{}
	if _, err := json.Marshal(o); err == nil {
		t.Fatal("marshalled an absent field without omitzero")
	}
}

func TestSchema(t *testing.T) {
	s := form.Of[itemID]{}.Schema()
	if s.Grammar != "test-item-id" || s.Pattern != "^Q[1-9][0-9]*$" || s.Example != "Q1" || s.Values != nil {
		t.Fatalf("minted schema %+v", s)
	}
	s = form.Of[level]{}.Schema()
	if s.Pattern != "" || strings.Join(s.Values, ",") != "low,high" || s.Example != "low" {
		t.Fatalf("vocabulary schema %+v", s)
	}
}

func TestRegisterRefusesDuplicateName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a second grammar with a taken name registered")
		}
	}()
	form.Register[impostor]()
}

type impostor struct{}

func (impostor) Name() string         { return "test-level" }
func (impostor) Check(s string) error { return nil }
func (impostor) Samples() []string    { return []string{"x"} }
