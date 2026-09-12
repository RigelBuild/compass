package store

// Reserved-prefix predicate contracts — pure functions, no Postgres. The
// admit/reject matrix over serverSecretPrefixes, including the COMPASS_ addition
// (the master-key prefix) and its case-variant reject. HasServerSecretPrefix is
// the byte-exact ADMIT check at the server door; ShadowsServerSecretPrefix is
// the case-folding REJECT check at the user door.

import "testing"

func TestHasServerSecretPrefixAdmits(t *testing.T) {
	admit := []string{
		"SERVER_FORGE_APP_PEM",
		"GATEWAY_CREDENTIALS_MASTER_KEY",
		"COMPASS_MASTER_KEY",
		MasterKeyName,
	}
	for _, n := range admit {
		if !HasServerSecretPrefix(n) {
			t.Errorf("HasServerSecretPrefix(%q) = false, want true", n)
		}
	}

	reject := []string{
		"PLAIN",
		"MASTER_KEY",          // bare, no reserved prefix
		"COMPASSX_MASTER_KEY", // near-miss: no underscore after COMPASS
		"compass_master_key",  // case variant: byte-exact admit must NOT match
		"Compass_Master_Key",  // mixed case
		"",
	}
	for _, n := range reject {
		if HasServerSecretPrefix(n) {
			t.Errorf("HasServerSecretPrefix(%q) = true, want false", n)
		}
	}
}

func TestShadowsServerSecretPrefixFoldsCompass(t *testing.T) {
	// Every case variant of COMPASS_ is rejected at the user door.
	reject := []string{"COMPASS_K", "compass_k", "CoMpAsS_k"}
	for _, n := range reject {
		if !ShadowsServerSecretPrefix(n) {
			t.Errorf("ShadowsServerSecretPrefix(%q) = false, want true", n)
		}
	}

	// A near-miss (no underscore) shares the letters but is not the prefix, so
	// neither predicate matches it — it is a legitimate user name.
	if ShadowsServerSecretPrefix("COMPASSX_K") {
		t.Error("ShadowsServerSecretPrefix(COMPASSX_K) = true, want false")
	}

	// The two predicates differ on the lowercase spelling: admitted byte-exact,
	// rejected case-fold.
	if HasServerSecretPrefix("compass_k") || !ShadowsServerSecretPrefix("compass_k") {
		t.Error("admit/reject predicates should differ on lowercase compass_k")
	}
}
