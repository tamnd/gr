package cred

import (
	"bytes"
	"testing"
)

// TestHashVerify confirms a freshly hashed password verifies and a wrong one does not,
// and that the salt and hash are non-empty so a zero record can never accidentally
// match.
func TestHashVerify(t *testing.T) {
	salt, hash, err := Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) == 0 || len(hash) == 0 {
		t.Fatalf("salt %d hash %d, want both non-empty", len(salt), len(hash))
	}
	rec := Record{Name: "ada", Salt: salt, Hash: hash, Iter: Iter()}
	if !Verify(rec, "correct horse") {
		t.Error("right password did not verify")
	}
	if Verify(rec, "wrong password") {
		t.Error("wrong password verified")
	}
}

// TestVerifyEmptyRecord confirms a record with no hash never verifies, so a decode of a
// foreign or zero blob cannot be coaxed into accepting a password.
func TestVerifyEmptyRecord(t *testing.T) {
	if Verify(Record{}, "") {
		t.Error("empty record verified the empty password")
	}
	if Verify(Record{Name: "x"}, "anything") {
		t.Error("record with no hash verified")
	}
}

// TestSaltIsRandom confirms two hashes of the same password draw different salts, so
// two users with the same password do not share a hash.
func TestSaltIsRandom(t *testing.T) {
	s1, h1, err := Hash("same")
	if err != nil {
		t.Fatal(err)
	}
	s2, h2, err := Hash("same")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(s1, s2) {
		t.Error("two hashes drew the same salt")
	}
	if bytes.Equal(h1, h2) {
		t.Error("two hashes of the same password matched")
	}
}

// TestValidRole confirms the four built-in roles validate and anything else does not.
func TestValidRole(t *testing.T) {
	for _, r := range []string{RoleReader, RoleEditor, RolePublisher, RoleAdmin} {
		if !ValidRole(r) {
			t.Errorf("%q did not validate", r)
		}
	}
	for _, r := range []string{"", "Reader", "owner", "superuser"} {
		if ValidRole(r) {
			t.Errorf("%q validated, want rejected", r)
		}
	}
}

// TestEncodeDecodeRoundTrip confirms a set of records survives a round trip through the
// wire format, names and roles and secrets intact, sorted by name regardless of input
// order.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := []Record{
		{Name: "zoe", Salt: []byte{1, 2}, Hash: []byte{3, 4, 5}, Iter: 1000, Roles: []string{RoleAdmin}},
		{Name: "ada", Salt: []byte{6}, Hash: []byte{7}, Iter: 2000, Roles: []string{RoleReader, RoleEditor}},
		{Name: "lin", Salt: nil, Hash: []byte{9}, Iter: 3000, Roles: nil},
	}
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d records, want 3", len(out))
	}
	if out[0].Name != "ada" || out[1].Name != "lin" || out[2].Name != "zoe" {
		t.Errorf("not sorted by name: %q %q %q", out[0].Name, out[1].Name, out[2].Name)
	}
	ada := out[0]
	if ada.Iter != 2000 || !bytes.Equal(ada.Hash, []byte{7}) {
		t.Errorf("ada round-tripped wrong: %+v", ada)
	}
	if len(ada.Roles) != 2 || ada.Roles[0] != RoleReader || ada.Roles[1] != RoleEditor {
		t.Errorf("ada roles = %v, want [reader editor]", ada.Roles)
	}
}

// TestDecodeEmpty confirms an empty blob decodes to an empty set, the state of a
// database that never created a user, rather than an error.
func TestDecodeEmpty(t *testing.T) {
	recs, err := Decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if recs != nil {
		t.Errorf("empty blob decoded to %v, want nil", recs)
	}
}

// TestDecodeBadMagic confirms a blob with the wrong leading byte is rejected as a
// foreign or corrupt section rather than parsed.
func TestDecodeBadMagic(t *testing.T) {
	if _, err := Decode([]byte{0x00, version, 0, 0, 0, 0}); err == nil {
		t.Error("a bad magic byte decoded without error")
	}
}

// TestDecodeTruncated confirms a blob cut short mid-record is rejected rather than
// returning a partial record.
func TestDecodeTruncated(t *testing.T) {
	full := Encode([]Record{{Name: "ada", Salt: []byte{1}, Hash: []byte{2}, Iter: 5, Roles: []string{RoleReader}}})
	if _, err := Decode(full[:len(full)-3]); err == nil {
		t.Error("a truncated blob decoded without error")
	}
}
