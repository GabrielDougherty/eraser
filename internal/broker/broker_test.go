package broker

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const sampleYAML = `brokers:
    - id: alpha
      name: Alpha Data
      email: privacy@alpha.example
      website: https://alpha.example
      opt_out_url: https://alpha.example/opt-out
      region: us
      category: people-search
    - id: bravo
      name: Bravo Marketing
      email: privacy@bravo.example
      region: eu
      category: marketing
    - id: charlie
      name: Charlie Global
      email: privacy@charlie.example
      region: global
      category: background-check
      requires_id: true
    - id: delta
      name: Delta NoEmail
      email: ""
      region: us
      category: people-search
`

func writeDB(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brokers.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func loadSample(t *testing.T) *BrokerDatabase {
	t.Helper()
	db, err := LoadFromFile(writeDB(t, sampleYAML))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	return db
}

func TestLoadFromFile(t *testing.T) {
	db := loadSample(t)

	if len(db.Brokers) != 4 {
		t.Fatalf("expected 4 brokers, got %d", len(db.Brokers))
	}
	first := db.Brokers[0]
	if first.ID != "alpha" || first.Name != "Alpha Data" || first.Email != "privacy@alpha.example" {
		t.Errorf("first broker parsed wrong: %+v", first)
	}
	if first.OptOutURL != "https://alpha.example/opt-out" {
		t.Errorf("opt_out_url = %q", first.OptOutURL)
	}
	if !db.Brokers[2].RequiresID {
		t.Error("requires_id: true did not parse")
	}
	if db.Brokers[1].RequiresID {
		t.Error("requires_id defaulted to true when absent")
	}
}

func TestLoadFromFileErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadFromFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("expected an error for a missing file")
		}
	})

	t.Run("malformed yaml", func(t *testing.T) {
		_, err := LoadFromFile(writeDB(t, "brokers:\n  - id: [unclosed\n"))
		if err == nil {
			t.Fatal("expected an error for malformed yaml")
		}
		if !strings.Contains(err.Error(), "parse") {
			t.Errorf("error should say the file failed to parse, got: %v", err)
		}
	})
}

// TestLoadFromFileSanitizesURLs covers the reason sanitizeBroker exists: these
// URLs are handed to a browser by `eraser fill`, so a javascript: or data:
// scheme arriving from the broker file must not survive loading. Anything
// that isn't http(s) is dropped rather than rejected, so one bad entry can't
// make the whole database unloadable.
func TestLoadFromFileSanitizesURLs(t *testing.T) {
	db, err := LoadFromFile(writeDB(t, `brokers:
    - id: js
      name: Script Scheme
      email: a@b.example
      website: "javascript:alert(1)"
      opt_out_url: "javascript:alert(2)"
      region: us
    - id: data
      name: Data Scheme
      email: c@d.example
      opt_out_url: "data:text/html,<script>x</script>"
      region: us
    - id: relative
      name: No Scheme
      email: e@f.example
      opt_out_url: "/opt-out"
      region: us
    - id: good
      name: Fine
      email: g@h.example
      website: http://plain.example
      opt_out_url: https://secure.example/opt-out
      region: us
`))
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	for _, id := range []string{"js", "data", "relative"} {
		b := db.FindByID(id)
		if b == nil {
			t.Fatalf("broker %q missing", id)
		}
		if b.Website != "" || b.OptOutURL != "" {
			t.Errorf("broker %q kept a non-http(s) URL: website=%q opt_out=%q", id, b.Website, b.OptOutURL)
		}
	}

	good := db.FindByID("good")
	if good.Website != "http://plain.example" {
		t.Errorf("http:// website was dropped: %q", good.Website)
	}
	if good.OptOutURL != "https://secure.example/opt-out" {
		t.Errorf("https:// opt-out URL was dropped: %q", good.OptOutURL)
	}
}

func TestIsValidURL(t *testing.T) {
	cases := map[string]bool{
		"":                          true, // absent is fine; only bad schemes are rejected
		"http://example.com":        true,
		"https://example.com/a?b=c": true,
		"HTTPS://EXAMPLE.COM":       true, // scheme comparison is case-insensitive
		"javascript:alert(1)":       false,
		"data:text/html,x":          false,
		"file:///etc/passwd":        false,
		"ftp://example.com":         false,
		"/relative/path":            false,
		"example.com":               false, // no scheme
		"://missing-scheme":         false,
	}
	for in, want := range cases {
		if got := isValidURL(in); got != want {
			t.Errorf("isValidURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFilter(t *testing.T) {
	db := loadSample(t)

	// Sorted so subtests can compare exactly rather than probing for the
	// absence of one entry - an assertion that only checks something is
	// missing would also pass if the filter returned nothing at all.
	ids := func(bs []Broker) []string {
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = b.ID
		}
		sort.Strings(out)
		return out
	}
	equal := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("no filters returns everything", func(t *testing.T) {
		equal(t, ids(db.Filter(nil, nil)), []string{"alpha", "bravo", "charlie", "delta"})
	})

	t.Run("region includes global brokers", func(t *testing.T) {
		// charlie is region "global", so it belongs to every region's run -
		// a global broker holds data on people everywhere.
		equal(t, ids(db.Filter([]string{"eu"}, nil)), []string{"bravo", "charlie"})
	})

	t.Run("region matching is case-insensitive", func(t *testing.T) {
		equal(t, ids(db.Filter([]string{"EU"}, nil)), []string{"bravo", "charlie"})
	})

	t.Run("excluded by id", func(t *testing.T) {
		equal(t, ids(db.Filter(nil, []string{"alpha"})), []string{"bravo", "charlie", "delta"})
	})

	t.Run("excluded by name, case-insensitively", func(t *testing.T) {
		equal(t, ids(db.Filter(nil, []string{"bravo marketing"})), []string{"alpha", "charlie", "delta"})
	})

	t.Run("exclusion beats region", func(t *testing.T) {
		equal(t, ids(db.Filter([]string{"eu"}, []string{"charlie"})), []string{"bravo"})
	})
}

// TestFilterRegionGlobalSelectsEverything pins a quirk worth knowing about
// before someone "fixes" it: asking for region "global" returns every broker,
// not just the ones marked global, because the region check treats a
// requested "global" as "no regional restriction".
func TestFilterRegionGlobalSelectsEverything(t *testing.T) {
	db := loadSample(t)
	if got := len(db.Filter([]string{"global"}, nil)); got != 4 {
		t.Errorf("region global returned %d brokers, expected all 4", got)
	}
}

func TestFindByID(t *testing.T) {
	db := loadSample(t)

	if b := db.FindByID("alpha"); b == nil || b.Name != "Alpha Data" {
		t.Errorf("FindByID(alpha) = %+v", b)
	}
	if b := db.FindByID("ALPHA"); b == nil {
		t.Error("FindByID should be case-insensitive")
	}
	if b := db.FindByID("nope"); b != nil {
		t.Errorf("FindByID(nope) = %+v, want nil", b)
	}

	// The returned pointer aliases the database, which callers rely on to
	// read a broker's live fields; make sure that stays true.
	db.FindByID("alpha").Notes = "touched"
	if db.Brokers[0].Notes != "touched" {
		t.Error("FindByID returned a copy; callers expect a pointer into the database")
	}
}

func TestFindByEmail(t *testing.T) {
	db := loadSample(t)

	if b := db.FindByEmail("privacy@bravo.example"); b == nil || b.ID != "bravo" {
		t.Errorf("FindByEmail = %+v", b)
	}
	if b := db.FindByEmail("PRIVACY@BRAVO.EXAMPLE"); b == nil {
		t.Error("FindByEmail should be case-insensitive")
	}
	if b := db.FindByEmail("nobody@example.com"); b != nil {
		t.Errorf("expected nil for an unknown address, got %+v", b)
	}
}

// TestEmptyEmailNeverMatches guards a sharp edge. 135 brokers in the shipped
// database have no email address at all, so a lookup for "" would otherwise
// match whichever of them happens to come first - and RemoveByEmail("")
// would delete it. The one caller (cleanup-bounces) already refuses to look
// up an empty address, but nothing should depend on that: an empty address is
// never a correct answer here.
func TestEmptyEmailNeverMatches(t *testing.T) {
	db := loadSample(t)
	before := len(db.Brokers)

	if b := db.FindByEmail(""); b != nil {
		t.Errorf("FindByEmail(\"\") matched %q; empty must never match", b.ID)
	}
	if b := db.RemoveByEmail(""); b != nil {
		t.Errorf("RemoveByEmail(\"\") removed %q; empty must never match", b.ID)
	}
	if len(db.Brokers) != before {
		t.Errorf("RemoveByEmail(\"\") changed the database: %d -> %d", before, len(db.Brokers))
	}
}

func TestRemoveByID(t *testing.T) {
	db := loadSample(t)

	removed := db.RemoveByID("BRAVO")
	if removed == nil || removed.ID != "bravo" {
		t.Fatalf("RemoveByID = %+v, want bravo", removed)
	}
	if len(db.Brokers) != 3 {
		t.Fatalf("expected 3 brokers left, got %d", len(db.Brokers))
	}
	if db.FindByID("bravo") != nil {
		t.Error("bravo is still findable after removal")
	}

	// Order of the survivors must be preserved - the file is human-edited
	// and reordering it would make every diff unreadable.
	if got := []string{db.Brokers[0].ID, db.Brokers[1].ID, db.Brokers[2].ID}; got[0] != "alpha" || got[1] != "charlie" || got[2] != "delta" {
		t.Errorf("removal reordered the database: %v", got)
	}

	// The returned broker must be a detached copy, not a window onto the
	// slice - after the removal that memory belongs to a different element.
	removed.Name = "mutated"
	if db.Brokers[1].Name == "mutated" {
		t.Error("the removed broker aliases a broker still in the database")
	}

	if db.RemoveByID("never-existed") != nil {
		t.Error("removing an unknown id should report nil")
	}
}

func TestRemoveByEmail(t *testing.T) {
	db := loadSample(t)

	removed := db.RemoveByEmail("PRIVACY@ALPHA.EXAMPLE")
	if removed == nil || removed.ID != "alpha" {
		t.Fatalf("RemoveByEmail = %+v, want alpha", removed)
	}
	if len(db.Brokers) != 3 || db.Brokers[0].ID != "bravo" {
		t.Errorf("unexpected database after removal: %d brokers, first=%q", len(db.Brokers), db.Brokers[0].ID)
	}
	if db.RemoveByEmail("nobody@example.com") != nil {
		t.Error("removing an unknown address should report nil")
	}
}

func TestAdd(t *testing.T) {
	db := loadSample(t)

	if err := db.Add(Broker{ID: "echo", Name: "Echo", Email: "e@example.com", Region: "us"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(db.Brokers) != 5 || db.FindByID("echo") == nil {
		t.Error("Add did not append the broker")
	}

	err := db.Add(Broker{ID: "echo", Name: "Duplicate", Email: "x@example.com"})
	if err == nil {
		t.Fatal("Add should refuse a duplicate ID")
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Errorf("error should name the offending ID, got: %v", err)
	}

	// Duplicate detection goes through FindByID, so it is case-insensitive.
	if err := db.Add(Broker{ID: "ECHO", Name: "Also Duplicate"}); err == nil {
		t.Error("Add should refuse a duplicate ID differing only in case")
	}
	if len(db.Brokers) != 5 {
		t.Errorf("rejected adds still changed the database: %d brokers", len(db.Brokers))
	}
}

func TestSaveRoundTrip(t *testing.T) {
	db := loadSample(t)
	if err := db.Add(Broker{ID: "echo", Name: "Echo", Email: "e@example.com", Region: "eu", Tags: []string{"new"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	path := filepath.Join(t.TempDir(), "out.yaml")
	if err := db.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("reloading what we just saved: %v", err)
	}
	if len(reloaded.Brokers) != len(db.Brokers) {
		t.Fatalf("round trip lost brokers: %d -> %d", len(db.Brokers), len(reloaded.Brokers))
	}

	echo := reloaded.FindByID("echo")
	if echo == nil || echo.Region != "eu" || len(echo.Tags) != 1 || echo.Tags[0] != "new" {
		t.Errorf("round trip mangled the added broker: %+v", echo)
	}
	// Empty optional fields must not reappear as empty keys that then fail
	// to round trip; delta has no email and must still have none.
	if d := reloaded.FindByID("delta"); d == nil || d.Email != "" {
		t.Errorf("delta's empty email did not survive the round trip: %+v", d)
	}
}

func TestSaveWithBackup(t *testing.T) {
	t.Run("preserves the previous contents", func(t *testing.T) {
		path := writeDB(t, sampleYAML)
		db, err := LoadFromFile(path)
		if err != nil {
			t.Fatalf("LoadFromFile: %v", err)
		}
		if db.RemoveByID("alpha") == nil {
			t.Fatal("fixture broker missing")
		}
		if err := db.SaveWithBackup(path); err != nil {
			t.Fatalf("SaveWithBackup: %v", err)
		}

		// The backup is the safety net for a destructive edit, so it must
		// hold what was there *before* the save, not after.
		backup, err := LoadFromFile(path + ".bak")
		if err != nil {
			t.Fatalf("reading backup: %v", err)
		}
		if backup.FindByID("alpha") == nil {
			t.Error("backup does not contain the broker that was removed")
		}

		current, err := LoadFromFile(path)
		if err != nil {
			t.Fatalf("reading saved file: %v", err)
		}
		if current.FindByID("alpha") != nil {
			t.Error("the removal was not actually saved")
		}
	})

	t.Run("works when there is nothing to back up", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.yaml")
		db := &BrokerDatabase{Brokers: []Broker{{ID: "solo", Name: "Solo", Email: "s@example.com"}}}
		if err := db.SaveWithBackup(path); err != nil {
			t.Fatalf("SaveWithBackup on a new path: %v", err)
		}
		if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
			t.Error("a backup was created for a file that did not exist")
		}
		if _, err := LoadFromFile(path); err != nil {
			t.Errorf("saved file is not loadable: %v", err)
		}
	})
}

func TestSaveErrors(t *testing.T) {
	// A path whose parent is a regular file: writing there fails with
	// ENOTDIR, which is the closest thing to a reliable unwritable path
	// that doesn't depend on running as an unprivileged user.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	unwritable := filepath.Join(blocker, "brokers.yaml")

	db := &BrokerDatabase{Brokers: []Broker{{ID: "a", Name: "A"}}}

	if err := db.Save(unwritable); err == nil {
		t.Error("Save should report a write failure")
	}
	if err := db.SaveWithBackup(unwritable); err == nil {
		t.Error("SaveWithBackup should report a write failure")
	}
}

// TestShippedDatabaseIsWellFormed loads the real broker database. It is the
// one input every send depends on and it is hand-edited, so a malformed entry
// is a live hazard rather than a hypothetical one - a duplicate ID would make
// FindByID return whichever came first, and a broker with no ID could never
// be addressed or excluded at all.
func TestShippedDatabaseIsWellFormed(t *testing.T) {
	const path = "../../data/brokers.yaml"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skipf("%s not present, skipping", path)
	}

	db, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("the shipped broker database does not load: %v", err)
	}
	if len(db.Brokers) == 0 {
		t.Fatal("the shipped broker database is empty")
	}

	seen := make(map[string]string, len(db.Brokers))
	for i, b := range db.Brokers {
		if strings.TrimSpace(b.ID) == "" {
			t.Errorf("broker %d (%q) has no id", i, b.Name)
			continue
		}
		if strings.TrimSpace(b.Name) == "" {
			t.Errorf("broker %q has no name", b.ID)
		}
		key := strings.ToLower(b.ID)
		if prev, dup := seen[key]; dup {
			t.Errorf("duplicate broker id %q (also used by %q); FindByID can only ever return the first", b.ID, prev)
		}
		seen[key] = b.Name
	}
}
