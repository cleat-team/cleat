package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// reseal-payloads command
// ---------------------------------------------------------------------------
//
// Why this exists. cleat#1776 bound payload ciphertexts to their tenant with
// GCM additional authenticated data, and #1792 shipped it. It could not bind
// the rows already on disk: those are sealed with nil AAD, and a nil-AAD blob
// authenticates nothing about its tenant, so it still decrypts when moved into
// another tenant's row. The read path keeps a fallback to that form because
// dropping it would make existing data unreadable -- and that fallback is the
// same door the substitution came through.
//
// So the binding is only complete once no legacy blob remains, and this is the
// command that gets there. Until it has run, #1792 is a fix for new writes and
// nothing else.
//
// WHY NOT A MIGRATION, which is the first thing anyone asks. A numbered
// migration has no key: the payload key is worker configuration, read from a
// file at startup (cmd/cleat-worker/main.go). Nothing under migrations/ can
// decrypt a payload, let alone re-seal one. This has to be a command run with
// the key in hand.
//
// PostgreSQL only, and that is the feature's scope rather than this command's
// laziness: --encrypt-sensitive-payloads is refused unless --driver=postgres
// (main.go:742), the encryptor is attached behind a type assertion to
// *engine.PostgresStoreFactory, and the encrypting write path's INSERT is
// Postgres syntax. The portedOn entry states it.

const resealPayloadsUsage = `usage: cleatctl reseal-payloads --encryption-key-file <path> [flags]

Re-seals payload ciphertexts written before cleat#1776 so that each is bound to
the tenant whose row it sits in.

  --encryption-key-file <path>   base64 payload key, same file the worker reads
  --dry-run                     report what would change, write nothing

Exit status is non-zero if anything was left unconverted, so this can be run to
completion in a loop and its exit code trusted.
`

// resealStats is the report, and every field exists because a sweep that says
// "done" without one is indistinguishable from a sweep that matched no rows.
type resealStats struct {
	Rows       int // event_history rows examined
	Older      int // column values found in an older form (legacy or bound)
	Rewrote    int // column values re-sealed and written
	Current    int // already the newest form: nothing to do
	NotCipher  int // values that are not ciphertext at all (plaintext, or "")
	Unreadable int // values that open in no form -- reported, never touched
}

// resealPayloads is the whole of the sweep, separated from flag parsing so a
// test can drive it against a real database.
//
// ONE CROSS-TENANT PASS, SEALING EACH ROW WITH ITS OWN tenant_id. An earlier
// draft iterated tenants and set cleat.tenant_id per tenant, so that it would
// also work on a connection row-level security applies to. That was the wrong
// shape: it needs a tenant LIST, and a tenant missing from that list
// contributes no rows, no errors and no findings -- the sweep reports clean and
// leaves unbound ciphertext behind. Reading tenant_id off the row removes the
// list, and with it the whole class.
//
// It is also what this tool already requires. printUsage says --db "must name a
// role that row-level security does not apply to: a superuser, or one with
// BYPASSRLS", because every other cleatctl command asks cluster-wide questions
// too. On a connection RLS DOES apply to, the first SELECT below raises --
// cleat.assert_tenant_set() is written to raise rather than filter to nothing --
// so a misconfigured role fails loudly here instead of converting one tenant
// and reporting success. TestResealRefusesAnRLSRestrictedConnection pins that.
func resealPayloads(ctx context.Context, db *sql.DB, enc *engine.PayloadEncryption,
	dryRun bool, out io.Writer) (resealStats, error) {

	var st resealStats

	cols := engine.EncryptedEventColumns
	//nolint:gosec // G202: the only concatenated fragment is quoteIdents(engine.EncryptedEventColumns), a package-level literal slice in this repo's own source with each element wrapped by quoteIdent. No caller-controlled string reaches this -- the command takes a key file path and two booleans, and nothing read from the database is spliced in. Same argument as engine/mysql_lifecycle.go:1138.
	sel := "SELECT workflow_id, step, tenant_id::text, " + strings.Join(quoteIdents(cols), ", ") +
		" FROM event_history ORDER BY workflow_id, step"
	rows, err := db.QueryContext(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("reading event_history: %w", err)
	}

	type pending struct {
		workflowID string
		step       int
		values     map[string]string
	}
	var work []pending

	for rows.Next() {
		var workflowID, tenant string
		var step int
		holders := make([]sql.NullString, len(cols))
		scanTo := []any{&workflowID, &step, &tenant}
		for i := range holders {
			scanTo = append(scanTo, &holders[i])
		}
		if err := rows.Scan(scanTo...); err != nil {
			rows.Close()
			return st, fmt.Errorf("scan: %w", err)
		}
		st.Rows++

		changed := map[string]string{}
		for i, col := range cols {
			if !holders[i].Valid || holders[i].String == "" {
				continue
			}
			next, kind, verr := resealValue(enc, tenant, holders[i].String)
			switch kind {
			case valueConverted:
				st.Older++
				changed[col] = next
			case valueCurrent:
				st.Current++
			case valueNotCiphertext:
				st.NotCipher++
			case valueUnreadable:
				st.Unreadable++
				fmt.Fprintf(out, "  UNREADABLE %s step=%d %s: %v\n", workflowID, step, col, verr)
			}
		}
		if len(changed) > 0 {
			work = append(work, pending{workflowID, step, changed})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return st, fmt.Errorf("iterate event_history: %w", err)
	}
	rows.Close()

	if dryRun {
		return st, nil
	}

	for _, w := range work {
		names := make([]string, 0, len(w.values))
		for c := range w.values {
			names = append(names, c)
		}
		sort.Strings(names) // deterministic SQL, so a failure is reproducible
		sets := make([]string, 0, len(names))
		args := []any{w.workflowID, w.step}
		for i, c := range names {
			sets = append(sets, fmt.Sprintf("%s = $%d", quoteIdent(c), i+3))
			args = append(args, w.values[c])
		}
		//nolint:gosec // G202: `sets` is built from the same engine.EncryptedEventColumns literal through quoteIdent, and every VALUE is bound as a $3.. parameter rather than interpolated; the two predicates are bound as $1 and $2. Nothing caller-controlled is concatenated.
		q := "UPDATE event_history SET " + strings.Join(sets, ", ") +
			" WHERE workflow_id = $1 AND step = $2"
		res, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return st, fmt.Errorf("event_history row %s step %d: write failed: %w", w.workflowID, w.step, err)
		}
		// Zero rows on an UPDATE of a row just read means readable but not
		// writable. Refusing is the point: continuing would count a rewrite
		// that did not happen, and the count is the only evidence the sweep
		// finished.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return st, fmt.Errorf("event_history row %s step %d matched no row on write: it was "+
				"read but not written, so this connection cannot complete the sweep",
				w.workflowID, w.step)
		}
		st.Rewrote += len(names)
	}

	return st, nil
}

type valueKind int

const (
	valueConverted     valueKind = iota // was an older form, now re-sealed
	valueCurrent                        // already the newest form
	valueNotCiphertext                  // plaintext, or not base64: leave alone
	valueUnreadable                     // base64 but opens in no form
)

// resealValue classifies one stored value and, when it is not already in the
// newest form, returns its replacement in the SAME stored form.
//
// TWO OLDER FORMS NOW, NOT ONE. cleat#1793 made the payload key per-tenant, so
// a value can be: derived-key + tenant AAD (current), master-key + tenant AAD
// (cleat#1792, and what THIS command wrote before #1793), or master-key + nil
// AAD (pre-#1792). The middle one is why a boolean "is it bound" stopped being
// enough -- this command wrote those itself, and they still need converting
// because one recovered key reads every tenant's payloads. engine.OpenAndClassify
// answers with the form rather than a yes/no.
//
// THE FORM IS SELF-DESCRIBING, which is why there is no per-column table here.
// Measured against a real row: the ten string columns hold bare base64 of the
// ciphertext and `payload` holds the same base64 wrapped in double quotes,
// because EncryptJSON writes a JSON string literal into a JSONB column. This
// detects the quoting and restores it, so a column cannot be rewritten in the
// wrong encoding by a table entry being wrong.
func resealValue(enc *engine.PayloadEncryption, tenant, stored string) (string, valueKind, error) {
	body, quoted := stored, false
	if len(stored) >= 2 && stored[0] == '"' && stored[len(stored)-1] == '"' {
		body, quoted = stored[1:len(stored)-1], true
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", valueNotCiphertext, nil
	}

	plain, form, err := enc.OpenAndClassify(tenant, raw)
	switch {
	case err != nil:
		// Opens in no form: plaintext that happens to be valid base64, or a
		// row sealed under a different key. Reported, not touched.
		return "", valueUnreadable, err
	case form == engine.PayloadFormDerived:
		return "", valueCurrent, nil
	}

	sealed, err := enc.Encrypt(tenant, plain)
	if err != nil {
		return "", valueUnreadable, err
	}

	// VERIFY BEFORE WRITING, and verify the FORM and not just the round trip. A
	// re-seal that corrupts a payload is worse than the hole it closes, because
	// the plaintext is gone and no backup of the ciphertext helps -- and a
	// re-seal that quietly produced an older form again would leave the sweep
	// reporting progress it did not make.
	back, gotForm, err := enc.OpenAndClassify(tenant, sealed)
	if err != nil {
		return "", valueUnreadable, fmt.Errorf("re-sealed value does not open: %w", err)
	}
	if gotForm != engine.PayloadFormDerived {
		return "", valueUnreadable, fmt.Errorf("re-sealed value is %s, not derived", gotForm)
	}
	if string(back) != string(plain) {
		return "", valueUnreadable, errors.New("re-sealed value does not round-trip")
	}

	next := base64.StdEncoding.EncodeToString(sealed)
	if quoted {
		next = `"` + next + `"`
	}
	return next, valueConverted, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteIdents(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = quoteIdent(s)
	}
	return out
}

func runResealPayloads(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("reseal-payloads", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", resealPayloadsUsage) }

	keyFile := fs.String("encryption-key-file", "", "file holding the base64 payload key")
	dryRun := fs.Bool("dry-run", false, "report what would change, write nothing")

	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if *keyFile == "" {
		fmt.Fprintf(os.Stderr, "error: --encryption-key-file is required\n\n%s", resealPayloadsUsage)
		osExit(2)
		return
	}
	keyBytes, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read key file: %v\n", err)
		osExit(1)
		return
	}
	enc, err := engine.NewPayloadEncryption(strings.TrimSpace(string(keyBytes)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	st, err := resealPayloads(ctx, db, enc, *dryRun, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	verb := "re-sealed"
	if *dryRun {
		verb = "would re-seal"
	}
	fmt.Printf("rows examined:      %d\n", st.Rows)
	fmt.Printf("values %-12s %d of %d in an older form\n", verb+":", st.Rewrote, st.Older)
	fmt.Printf("already current:    %d\n", st.Current)
	fmt.Printf("not ciphertext:     %d\n", st.NotCipher)
	fmt.Printf("unreadable:         %d\n", st.Unreadable)

	// Non-zero while anything is left, so this can be looped on and its exit
	// code trusted. A dry run that found work is also non-zero: it is a report
	// that the database is not converted.
	if st.Unreadable > 0 || (*dryRun && st.Older > 0) {
		osExit(1)
		return
	}
}
