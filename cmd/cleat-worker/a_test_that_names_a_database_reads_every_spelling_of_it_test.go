package main

import "testing"

// cleat#3618. TestAuthMiddlewareRejectsInvalidKey read CLEAT_TEST_DB and DURABLE_TEST_DB but not
// CLEAT_TEST_POSTGRES, so for anyone who set only the last it skipped silently while its neighbours ran.
// authTestDSN is the one place that decides which variable it reads, and this pins it: each spelling alone is
// "configured", the order matches testutil.PostgresTestDSN, and an empty environment is the one skip.
func TestTheAuthTestReadsEverySpellingOfTheTestDatabase(t *testing.T) {
	const (
		pg      = "postgres://from-clean-test-postgres/x"
		db      = "postgres://from-clean-test-db/x"
		durable = "postgres://from-durable-test-db/x"
	)
	clear := func(t *testing.T) {
		t.Helper()
		t.Setenv("CLEAT_TEST_POSTGRES", "")
		t.Setenv("CLEAT_TEST_DB", "")
		t.Setenv("DURABLE_TEST_DB", "")
	}

	for _, tc := range []struct {
		name           string
		set            map[string]string
		wantDSN        string
		wantConfigured bool
	}{
		{"only CLEAT_TEST_POSTGRES (the case that used to skip)", map[string]string{"CLEAT_TEST_POSTGRES": pg}, pg, true},
		{"only CLEAT_TEST_DB", map[string]string{"CLEAT_TEST_DB": db}, db, true},
		{"only the deprecated DURABLE_TEST_DB", map[string]string{"DURABLE_TEST_DB": durable}, durable, true},
		{"CLEAT_TEST_POSTGRES wins over CLEAT_TEST_DB, as in testutil.PostgresTestDSN",
			map[string]string{"CLEAT_TEST_POSTGRES": pg, "CLEAT_TEST_DB": db}, pg, true},
		{"CLEAT_TEST_DB wins over the deprecated spelling",
			map[string]string{"CLEAT_TEST_DB": db, "DURABLE_TEST_DB": durable}, db, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clear(t)
			for k, v := range tc.set {
				t.Setenv(k, v)
			}
			got, configured := authTestDSN()
			if got != tc.wantDSN || configured != tc.wantConfigured {
				t.Errorf("authTestDSN() = %q, %v; want %q, %v", got, configured, tc.wantDSN, tc.wantConfigured)
			}
		})
	}

	// The negative control: with nothing set the answer is "not configured", which is the ONLY case the test
	// may skip on. Without it every row above passes against a function that always says configured.
	t.Run("nothing set is not configured", func(t *testing.T) {
		clear(t)
		if got, configured := authTestDSN(); configured {
			t.Errorf("authTestDSN() = %q, configured; want not configured when no variable is set", got)
		}
	})
}
