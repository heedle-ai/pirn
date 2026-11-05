package pirn

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The person's details (roadmap 9.8): kept in the keychain through the
// worker's vault, one item each, and given to an act step only as its
// program names them (:uses "name email"). Claude sees placeholders
// (Placeholder); the browser types the values (reed's UseProfile).

// ProfileFields are the details a person can keep, in the order shown.
var ProfileFields = []string{"name", "email", "phone", "address", "birthdate"}

// ProfileLabels are the fields in words.
var ProfileLabels = map[string]string{"name": "name", "email": "email", "phone": "phone number", "address": "address", "birthdate": "date of birth"}

// Uses is the option that names the details an act step may use, as one
// string of field names: (act "…" "https://…" :uses "name email").
const Uses = ":uses"

// MaxProfileValue bounds a detail, in bytes.
const MaxProfileValue = 256

// Placeholder is what Claude types for field: the browser types the value.
func Placeholder(field string) string { return "⟨" + field + "⟩" }

// ProfileKey is the keychain name a field is kept under.
func ProfileKey(field string) string { return "HEEDLE_PROFILE_" + strings.ToUpper(field) }

// CheckProfile reports whether field is a detail and value one pirn keeps:
// one line of 1 to MaxProfileValue bytes of text, no control characters.
func CheckProfile(field, value string) error {
	if !slices.Contains(ProfileFields, field) {
		return fmt.Errorf("%w: no detail %q (there are %s)", ErrInvalid, field, strings.Join(ProfileFields, ", "))
	}
	if strings.TrimSpace(value) == "" || len(value) > MaxProfileValue || !utf8.ValidString(value) {
		return fmt.Errorf("%w: your %s is 1 to %d bytes of text", ErrInvalid, ProfileLabels[field], MaxProfileValue)
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Errorf("%w: your %s is one line, without control characters", ErrInvalid, ProfileLabels[field])
		}
	}
	return nil
}

// ProfileVault is a vault that keeps details: the Keychain.
type ProfileVault interface {
	Vault
	Store(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
}

// StoreProfile keeps value as field in v. The keychain keeps a value as
// printable ASCII (CheckSecret), so it's kept base64url-encoded.
func StoreProfile(ctx context.Context, v ProfileVault, field, value string) error {
	if err := CheckProfile(field, value); err != nil {
		return err
	}
	return v.Store(ctx, ProfileKey(field), base64.RawURLEncoding.EncodeToString([]byte(value)))
}

// DeleteProfile forgets field.
func DeleteProfile(ctx context.Context, v ProfileVault, field string) error {
	if !slices.Contains(ProfileFields, field) {
		return fmt.Errorf("%w: no detail %q", ErrInvalid, field)
	}
	return v.Delete(ctx, ProfileKey(field))
}

// LookupProfile is field's value in v, and whether it is set. Only the
// environment never holds details: they're the person's, not a worker's.
func LookupProfile(ctx context.Context, v Vault, field string) (string, bool, error) {
	if !slices.Contains(ProfileFields, field) || v == nil {
		return "", false, nil
	}
	enc, ok, err := v.Lookup(ctx, ProfileKey(field))
	if err != nil || !ok {
		return "", false, err
	}
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || CheckProfile(field, string(b)) != nil {
		return "", false, fmt.Errorf("%w: the keychain's %s isn't one heedle kept: set it again on the Connectors page", ErrSetup, ProfileKey(field))
	}
	return string(b), true, nil
}

// ProfileFrom is Act.Profile for a worker's vault.
func ProfileFrom(v Vault) func(context.Context, string) (string, bool, error) {
	return func(ctx context.Context, field string) (string, bool, error) { return LookupProfile(ctx, v, field) }
}

// parseUses reads :uses' value: field names, space-separated, each once.
func parseUses(s string) ([]string, error) {
	var out []string
	for _, f := range strings.Fields(s) {
		if !slices.Contains(ProfileFields, f) || slices.Contains(out, f) {
			return nil, fmt.Errorf("%w: %s %q: name details once each, from %s", ErrInvalid, Uses, s, strings.Join(ProfileFields, ", "))
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s names no details", ErrInvalid, Uses)
	}
	return out, nil
}

// profileRule tells Claude the placeholders it may type.
func profileRule(uses []string) string {
	var b strings.Builder
	b.WriteString("\n- You may fill in these details of the person's, and no others. You never see them: type the placeholder exactly, as the whole value of a text field, and the browser types the real value. The page shows the placeholder in its place.\n")
	for _, f := range uses {
		fmt.Fprintf(&b, "  - %s: their %s\n", Placeholder(f), ProfileLabels[f])
	}
	return b.String()
}
