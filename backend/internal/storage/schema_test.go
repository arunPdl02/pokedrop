package storage

import "testing"

func TestTableNamesFromEnv(t *testing.T) {
	t.Setenv(EnvInventoryTable, "inv-table")
	t.Setenv(EnvReservationsTable, "res-table")

	got, err := TableNamesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Inventory != "inv-table" || got.Reservations != "res-table" {
		t.Errorf("unexpected table names: %+v", got)
	}
}

func TestTableNamesFromEnvMissing(t *testing.T) {
	cases := []struct{ inv, res string }{
		{"", "res-table"},
		{"inv-table", ""},
		{"", ""},
	}
	for _, c := range cases {
		t.Setenv(EnvInventoryTable, c.inv)
		t.Setenv(EnvReservationsTable, c.res)
		if _, err := TableNamesFromEnv(); err == nil {
			t.Errorf("expected error for inv=%q res=%q", c.inv, c.res)
		}
	}
}
