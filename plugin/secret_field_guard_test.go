package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This is the guard for the defect plugin.Secret was introduced to fix.
//
// Five plugins independently declared a credential as a plain `string` field on
// a struct that a list or get handler marshals straight to the caller:
// webhookingest and notifications (HMAC signing keys), pagerdutyalert (routing
// key), slacknotify (webhook URL, which IS the credential), datadogexport (API
// key). Nothing failed. The response looked correct in review, because the field
// was populated.
//
// datadogexport is the reason this is a type and a guard rather than a helper.
// It DID redact -- and the redaction was defeated by appending
// ?show_api_key=true, with no authorization behind it. A convention that has to
// be remembered, and a redaction that has an off switch, are the two shapes
// that already failed here.
//
// So: a field whose name says it holds a credential must be typed
// plugin.Secret, which cannot be marshaled in the clear. This test fails at
// authoring time, needs no database, and runs in every job.
//
// It is deliberately name-driven. A guard that only checked fields already
// known to be secrets would catch nothing new, and the next credential will
// arrive called api_key or signing_secret like all the others did.
func TestPluginCredentialFieldsUseTheSecretType(t *testing.T) {
	// Substrings that mean "this field holds something that authenticates".
	// Matched against the Go field name and the json tag, lowercased.
	credentialish := []string{
		"secret", "password", "passwd", "api_key", "apikey",
		"credential", "token", "private_key", "privatekey",
		"access_key", "accesskey", "signing_key", "signingkey",
		"routing_key", "routingkey", "webhook_url", "webhookurl",
		// A DSN/connection string commonly embeds a password (e.g.
		// postgres://user:password@host/db) -- scheduledbackup.Config.DSN
		// was exactly this, held as a plain string and passed to pg_dump as
		// a literal argv element, visible via ps and /proc/*/cmdline to any
		// co-resident user.
		"dsn",
	}

	// Reviewed exceptions. Adding one is a deliberate act; an empty reason is
	// not accepted.
	//
	// There are exactly two legitimate reasons, and they are different claims:
	//
	//   NOT A CREDENTIAL -- the name matched but the value does not
	//     authenticate anything. An AWS access key ID is an identifier, like a
	//     username.
	//
	//   CANNOT REACH A CALLER -- it IS a credential, but it is never marshaled
	//     towards one. Process config read from a deployment file is the case
	//     here: it is an input, and marshaling it is something only tests do.
	//     These are still worth converting eventually, for the logging path, and
	//     saying so honestly is better than pretending they are not credentials.
	//
	// A third reason -- "converting it broke something" -- is not on the list.
	// If a credential must be marshaled in the clear, that is what the second
	// pagerdutyalert entry documents: it is marshaled towards the service it
	// authenticates to, not towards our caller.
	type exception struct{ reason string }
	allowed := map[string]exception{
		// The PUBLIC half of an AWS key pair. It identifies the caller the way
		// a username does; the SecretAccessKey beside it is the credential and
		// IS a plugin.Secret. Redacting an access key ID would obscure which
		// identity a misconfigured deployment is using without protecting
		// anything.
		"blobstore.Config.AccessKeyID": {reason: "public half of an AWS key pair, not a secret"},

		// NOT A CREDENTIAL. token_endpoint is a URL out of an issuer's OIDC
		// discovery document -- where cleat should POST to exchange a code --
		// and it is published at a well-known path for anyone to read. It
		// matches here on the word "token" alone.
		//
		// The name is not ours to change: "token_endpoint" is the field name
		// in OpenID Connect Discovery, so renaming it out of this guard's way
		// would mean the JSON no longer decodes. Narrowed to this one field
		// rather than the type, so the ClientSecret-shaped things this guard
		// exists for stay in scope if discoveryDoc ever grows one. cleat#1582.
		"oauthprovider.discoveryDoc.TokenEndpoint": {
			reason: "a public URL from OIDC discovery, not a credential; the field name is fixed by the OIDC spec",
		},

		// This is the OUTBOUND request body to PagerDuty's Events API, not a
		// response to one of our callers. plugin.Secret would be exactly wrong
		// here: MarshalJSON would put "[redacted]" in the routing_key field and
		// every alert would fail authentication.
		//
		// Worth keeping as the worked example of why this allowlist exists.
		// The rule is "a credential must not be marshaled towards OUR caller",
		// not "a credential must never be marshaled" -- a credential has to
		// reach the service it authenticates to. The value arrives here from
		// pdConfigJSON.RoutingKey, which IS a plugin.Secret, via .Reveal().
		"pagerdutyalert.pdEventRequest.RoutingKey": {
			reason: "outbound body to PagerDuty; redacting it would break authentication",
		},

		// CANNOT REACH A CALLER. Process configuration, unmarshaled from a
		// deployment file into a Config struct and never returned by any
		// endpoint -- which is why they are not part of the leak this change
		// fixes.
		//
		// email.Config.SendGridAPIKey, llm.ProviderConfig.APIKey and
		// slacknotify.Config.SlackSigningSecret are GONE from this list,
		// not converted: cleat#1992 part 1 removed the first two fields from
		// Config/ProviderConfig, and cleat#2172 (part 1b) removed the third
		// from slacknotify's Config the same way -- each moving its
		// credential to a deployment secret. What remains in --plugin-config
		// is legacyEmailConfig.SendGridAPIKey, legacyProviderConfig.APIKey
		// and legacySlackConfig.SlackSigningSecret, read once at Init purely
		// to WARN or refuse to boot on a leftover value -- and those ARE
		// plugin.Secret (cleat-review's #2202 re-check for the first two,
		// the same convention followed for the third), not allowlisted,
		// since converting them cost nothing (none is built from a
		// marshaled Config struct in any test helper the way the three
		// removed entries were).
		//
		// blobstore.Config.SecretAccessKey is still worth converting, for the
		// logging path rather than the response path. That was attempted here
		// and backed out: plugin.Secret does not round-trip through
		// json.Marshal by design, and test helpers build their
		// Environment.Config by marshaling a Config struct. Production never
		// does that -- PluginLoader.DeployPlugin marshals a map[string]any
		// parsed from deployment JSON -- so the conversion is safe, but the
		// test churn belongs to its own change rather than riding along with a
		// security fix. Tracked in tiers.yaml under the plugins entry.
		"blobstore.Config.SecretAccessKey":          {reason: "process config; never returned by an endpoint (tracked)"},
		"oauthprovider.oauthConfigRow.ClientSecret": {reason: "internal row struct; handleListSessions never selects it (tracked)"},

		// CANNOT REACH A CALLER, the same shape as llm.ProviderConfig.APIKey
		// two lines up. chatRequest is decode-only in production -- it is
		// unmarshaled FROM a workflow's inputJSON in p.chat/p.chatStream and
		// never marshaled back out by any of this plugin's own code; the only
		// call sites that marshal it are test helpers building synthetic
		// input (per_tenant_api_key_test.go, host_functions_test.go,
		// llm_behavioral_test.go). plugin.Secret would break every one of
		// them the same way it was found to break llm.ProviderConfig.APIKey's
		// own test helpers: it does not round-trip through json.Marshal by
		// design, so `json.Marshal(chatRequest{APIKey: "k"})` would produce
		// `"api_key":"[redacted]"`, which UnmarshalJSON then refuses to
		// accept back.
		//
		// A literal key typed directly into workflow input (as opposed to a
		// resolved ${secret:...} reference) USED TO BE recorded in the clear
		// in event history -- cleat#1988's own PR (#2023) accepted this
		// rather than guess at a field's secrecy from its JSON shape, because
		// this plugin sees only the POST-RESOLUTION string and cannot tell a
		// resolved secret from a literal.
		//
		// cleat#2043 closed that gap, but NOT by converting this field's Go
		// type -- event history records the RAW inputJSON the workflow sent,
		// not a re-marshal of this struct, so that type has no bearing on
		// what gets stored, same as before. The fix is
		// plugin.FuncOptions.SecretOnlyFields: "chat" and "chat_stream" now
		// declare "api_key" secret-only, and the engine refuses a call whose
		// raw api_key isn't exactly a ${secret:NAME} reference before the
		// call is dispatched or recorded (engine/plugins.go,
		// checkSecretOnlyFields). This allowlist entry stays: that guard
		// checks something unrelated to #2043's fix (a credential-shaped
		// field marshaled back OUT toward a caller, and chatRequest still
		// never is), and this field still can't round-trip through
		// json.Marshal as plugin.Secret, for the same test-helper reason as
		// before.
		"llm.chatRequest.APIKey": {
			reason: "decode-only in production, never marshaled towards a caller; the literal-" +
				"secret gap this note used to describe was closed by cleat#2043's " +
				"SecretOnlyFields, which is enforced on the raw JSON, not on this Go type",
		},
	}

	root := filepath.Join("..", "plugins")
	fset := token.NewFileSet()
	var findings []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		pkg := f.Name.Name

		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				jsonTag := ""
				if field.Tag != nil {
					jsonTag = structTagValue(field.Tag.Value, "json")
				}
				// A field with no json tag is never marshaled by name, but it
				// can still be marshaled: encoding/json falls back to the Go
				// field name for exported fields. So both are checked.
				for _, name := range field.Names {
					if !name.IsExported() && jsonTag == "" {
						continue
					}
					hay := strings.ToLower(name.Name + " " + jsonTag)
					if !containsAny(hay, credentialish) {
						continue
					}
					if isSecretType(field.Type) {
						continue
					}
					// A credential is a string. This rule is what separates
					// "api_key" from "max_tokens", "prompt_tokens" and
					// "cost_per_1k_tokens" -- the LLM plugin counts tokens, and
					// a name-only match flagged ten of those on the first run.
					// A credential held as an int is not a thing.
					if !isStringType(field.Type) {
						continue
					}
					key := pkg + "." + ts.Name.Name + "." + name.Name
					if ex, ok := allowed[key]; ok {
						if strings.TrimSpace(ex.reason) == "" {
							findings = append(findings, key+
								" is allowlisted with no reason; an exception without evidence is not one")
						}
						continue
					}
					findings = append(findings, fset.Position(name.Pos()).String()+
						"  "+key+" (json:\""+jsonTag+"\") is "+typeString(field.Type)+
						", not plugin.Secret")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(findings) > 0 {
		sort.Strings(findings)
		t.Errorf("%d credential-shaped field(s) are not plugin.Secret:\n  %s\n\n"+
			"A plain string field holding a credential is returned in the clear by any handler "+
			"that marshals the struct, which is how five plugins leaked one. Use plugin.Secret: "+
			"it cannot be marshaled unredacted, and .Reveal() makes every intentional use "+
			"greppable.\n\nIf the field genuinely does not hold a credential, add it to the "+
			"allowlist in this test WITH A REASON.",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// isStringType reports whether the field is a plain string (or pointer/slice of
// one). Named string types other than Secret count too -- a credential wrapped
// in a domain type is still a credential.
func isStringType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "string"
	case *ast.StarExpr:
		return isStringType(t.X)
	case *ast.ArrayType:
		return isStringType(t.Elt)
	}
	return false
}

func containsAny(hay string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

// isSecretType reports whether the field's type is plugin.Secret, Secret (from
// inside the plugin package), or a pointer/slice of one.
func isSecretType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "Secret"
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "Secret"
	case *ast.StarExpr:
		return isSecretType(t.X)
	case *ast.ArrayType:
		return isSecretType(t.Elt)
	}
	return false
}

func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	}
	return "?"
}

// structTagValue pulls one key out of a raw struct tag literal, without
// depending on reflect.StructTag's exact quoting rules at parse time.
func structTagValue(raw, key string) string {
	raw = strings.Trim(raw, "`")
	for _, part := range strings.Fields(raw) {
		if !strings.HasPrefix(part, key+":\"") {
			continue
		}
		v := strings.TrimPrefix(part, key+":\"")
		v = strings.TrimSuffix(v, "\"")
		if i := strings.Index(v, ","); i >= 0 {
			v = v[:i]
		}
		return v
	}
	return ""
}
