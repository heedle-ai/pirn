package pirn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"sort"
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
func ProfileKey(field string) string { return profilePrefix + strings.ToUpper(field) }

const profilePrefix = "HEEDLE_PROFILE_"

// IsProfileName reports whether a secret's name is one a detail is kept
// under: no connector may name one, and the secret commands leave them be
// (review 42), so a detail only ever reaches an act step that names it.
func IsProfileName(name string) bool { return strings.HasPrefix(strings.ToUpper(name), profilePrefix) }

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

// redactor hides the person's details in whatever reaches Claude or the
// record (review 42): Playwright MCP hides only a value's exact copy, and
// a page can show one encoded, reformatted or spelled out. Each value is
// matched, ignoring case, as itself; URL-, form-, JSON-, HTML- and
// base64-encoded; and as its letters and digits with up to three other
// characters between each (spaced out, punctuated, or split), and is
// replaced by its placeholder.
type redactor struct {
	re []*regexp.Regexp
	ph []string
	// fields hide short text a person typed (roadmap 9.10) where it is a
	// field's whole value in a page outline: too short to hide everywhere.
	fields []*regexp.Regexp
}

func newRedactor(secrets map[string]string) redactor {
	var r redactor
	phs := make([]string, 0, len(secrets))
	for ph := range secrets {
		phs = append(phs, ph)
	}
	sort.Slice(phs, func(i, j int) bool { return len(secrets[phs[i]]) > len(secrets[phs[j]]) })
	for _, ph := range phs {
		v := secrets[ph]
		js, _ := json.Marshal(v)
		forms := []string{v, url.QueryEscape(v), url.PathEscape(v), strings.ReplaceAll(url.QueryEscape(v), "+", "%20"),
			strings.Trim(string(js), `"`), html.EscapeString(v),
			base64.StdEncoding.EncodeToString([]byte(v)), base64.RawStdEncoding.EncodeToString([]byte(v)),
			base64.URLEncoding.EncodeToString([]byte(v)), base64.RawURLEncoding.EncodeToString([]byte(v))}
		var alts []string
		for _, f := range forms {
			alts = append(alts, regexp.QuoteMeta(f))
		}
		var chars []string
		for _, c := range strings.ToLower(v) {
			if unicode.IsLetter(c) || unicode.IsDigit(c) {
				chars = append(chars, regexp.QuoteMeta(string(c)))
			}
		}
		if len(chars) >= 3 {
			alts = append(alts, strings.Join(chars, `[^\pL\pN]{0,3}`))
		}
		r.re = append(r.re, regexp.MustCompile(`(?i)(?:`+strings.Join(alts, "|")+`)`))
		r.ph = append(r.ph, ph)
	}
	return r
}

// text is s with every detail hidden.
func (r redactor) text(s string) string {
	for i, re := range r.re {
		s = re.ReplaceAllLiteralString(s, r.ph[i])
	}
	for _, re := range r.fields {
		s = re.ReplaceAllString(s, "${1}"+TypedPlaceholder+"${2}")
	}
	return s
}

// SignInPlaceholder stands for a value a loaded sign-in holds (a cookie's
// or local storage's), wherever a page shows it (roadmap 9.11).
const SignInPlaceholder = "⟨your sign-in⟩"

// withSignIn is r hiding the values a step's loaded sign-ins hold too,
// everywhere, as a detail is (review 53).
func (r redactor) withSignIn(values []string) redactor {
	for _, v := range values {
		x := newRedactor(map[string]string{SignInPlaceholder: v})
		r.re, r.ph = append(r.re, x.re...), append(r.ph, x.ph...)
	}
	return r
}

// withTyped is r hiding what a person typed while driving too (roadmap
// 9.10): each run of at least MinTyped characters everywhere, as a detail
// is, and a shorter one where it is a field's whole value in a page
// outline or a tool's input.
func (r redactor) withTyped(runs []string) redactor {
	// Longest first, so a value containing a shorter one is hidden whole
	// (review 52).
	runs = slices.Clone(runs)
	sort.SliceStable(runs, func(i, j int) bool { return len(runs[i]) > len(runs[j]) })
	for _, t := range runs {
		if len([]rune(t)) >= MinTyped {
			x := newRedactor(map[string]string{TypedPlaceholder: t})
			r.re, r.ph = append(r.re, x.re...), append(r.ph, x.ph...)
			continue
		}
		q := regexp.QuoteMeta(t)
		r.fields = append(r.fields,
			regexp.MustCompile(`(?m)(^\s*- (?:textbox|searchbox|spinbutton|combobox)\b[^\n]*?: )`+q+`()$`),
			regexp.MustCompile(`("(?:value|text)":\s*")`+q+`(")`))
	}
	return r
}

// result hides every detail in what an act step records.
func (r redactor) result(res *ActResult) {
	if len(r.re) == 0 && len(r.fields) == 0 {
		return
	}
	res.Text, res.URL = r.text(res.Text), r.text(res.URL)
	for i := range res.Calls {
		c := &res.Calls[i]
		c.Input, c.Output, c.URL = r.text(c.Input), r.text(c.Output), r.text(c.URL)
	}
	for i := range res.Blocked {
		res.Blocked[i] = r.text(res.Blocked[i])
	}
	for i := range res.Script {
		st := &res.Script[i]
		st.Input, st.Outline = r.text(st.Input), r.text(st.Outline)
		for j := range st.Targets {
			st.Targets[j].Name = r.text(st.Targets[j].Name)
		}
	}
	if res.Replay != nil {
		res.Replay.Why = r.text(res.Replay.Why)
	}
	for i := range res.Held {
		res.Held[i] = r.text(res.Held[i])
	}
	for i := range res.SentByYou {
		res.SentByYou[i] = r.text(res.SentByYou[i])
	}
	if res.Pending != nil {
		p := *res.Pending
		p.Summary, p.URL = r.text(p.Summary), r.text(p.URL)
		res.Pending = &p
	}
}
